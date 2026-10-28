package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RecheckRestoresProvenBundleOnce(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	beforeUnits, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("expected blocked accounting: %v", err)
	}
	buckets := []string{BucketUsageEvents, BucketUsageAggHourly, BucketQuotaUsageAggHourly, BucketUsageReceipts}
	beforeDigests := make(map[string][32]byte)
	if err := db.View(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			digest, err := backfillSourceDigest(tx, name)
			if err != nil {
				return err
			}
			beforeDigests[name] = digest
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "12345678-1234-4234-8234-123456789abc", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || !result.AccountingVerified || !result.AccountingRestored {
		t.Fatalf("restore: %+v %v", result, err)
	}
	audits, err := NewAuditStore(db).List(10)
	if err != nil || len(audits) != 1 || audits[0].Actor != "admin" || audits[0].Result != "accounting_restored" {
		t.Fatalf("restoration audit: %+v %v", audits, err)
	}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || len(units) != len(usage.ExpandEvent(event)) || !reflect.DeepEqual(units, beforeUnits) {
		t.Fatalf("accounting still blocked or rows changed: %+v %v", units, err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			digest, err := backfillSourceDigest(tx, name)
			if err != nil {
				return err
			}
			if digest != beforeDigests[name] {
				t.Errorf("ledger bucket %s changed", name)
			}
		}
		b := tx.Bucket([]byte(unresolvedBucket))
		_, raw := b.Cursor().First()
		var item unresolvedUsage
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		if !item.Resolved || !bytes.Equal(item.Original, original) {
			t.Error("original evidence changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry, err := store.RecheckUsage(req)
			if err != nil || retry != result {
				t.Errorf("retry: %+v %v", retry, err)
			}
		}()
	}
	wg.Wait()
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store = clockedStore(db)
	retry, err := store.RecheckUsage(req)
	if err != nil || retry != result {
		t.Fatalf("restart retry: %+v %v", retry, err)
	}
}

func TestIssue72RecheckRejectsAmbiguousOriginal(t *testing.T) {
	for _, kind := range []string{"duplicate", "case_collision", "case_variant", "nested_duplicate", "nested_case_variant", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedIdentityBundle()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(event)
			var original string
			switch kind {
			case "duplicate":
				original = strings.Replace(string(raw), `"client_name":`, `"client_name":"untrusted","client_name":`, 1)
			case "case_collision":
				original = strings.Replace(string(raw), `"client_name":`, `"CLIENT_NAME":"untrusted","client_name":`, 1)
			case "case_variant":
				original = strings.Replace(string(raw), `"client_name":`, `"CLIENT_NAME":`, 1)
			case "nested_duplicate":
				original = strings.Replace(string(raw), `"iteration_index":`, `"iteration_index":999,"iteration_index":`, 1)
			case "nested_case_variant":
				original = strings.Replace(string(raw), `"iteration_index":`, `"ITERATION_INDEX":`, 1)
			case "unknown":
				original = strings.Replace(string(raw), `{`, `{"untrusted":true,`, 1)
			}
			if original == string(raw) {
				t.Fatal("fixture did not change")
			}
			if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, []byte(original)) }); err != nil {
				t.Fatal(err)
			}
			entries, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries: %+v %v", entries, err)
			}
			result, err := store.RecheckUsage(UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "22345678-1234-4234-8234-123456789abc", Actor: "admin"})
			if err != nil || result.AccountingVerified || result.AccountingRestored || result.Status != "insufficient_evidence" {
				t.Fatalf("ambiguous proof: %+v %v", result, err)
			}
			after, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || after[0].Resolved || after[0].Version != entries[0].Version {
				t.Fatalf("state changed: %+v %v", after, err)
			}
		})
	}
}

func TestIssue72RecheckOnlyRestoresSelectedEvidenceAndOldReceiptDoesNotReplay(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := putUnresolvedUsage(tx, event, original); err != nil {
			return err
		}
		return putUnresolvedUsage(tx, event, append([]byte(" "), original...))
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "32345678-1234-4234-8234-123456789abc", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || !result.AccountingRestored {
		t.Fatalf("restore: %+v %v", result, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("second evidence did not block: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(unresolvedBucket))
		return b.ForEach(func(k, v []byte) error {
			if recheckHash(k) != req.EvidenceID {
				return nil
			}
			var item unresolvedUsage
			if err := json.Unmarshal(v, &item); err != nil {
				return err
			}
			item.Resolved = false
			value, err := json.Marshal(item)
			if err != nil {
				return err
			}
			return b.Put(k, value)
		})
	}); err != nil {
		t.Fatal(err)
	}
	retry, err := store.RecheckUsage(req)
	if err != nil || retry != result {
		t.Fatalf("old receipt retry: %+v %v", retry, err)
	}
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 2 || after[0].Resolved || after[1].Resolved {
		t.Fatalf("receipt restored state again: %+v %v", after, err)
	}
}

func TestIssue72RecheckVerifiedAlreadyResolvedIsNotRestored(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 || !entries[0].Resolved {
		t.Fatalf("fixture: %+v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "52345678-1234-4234-8234-123456789abc", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || !result.AccountingVerified || result.AccountingRestored {
		t.Fatalf("already resolved: %+v %v", result, err)
	}
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 1 || after[0] != entries[0] {
		t.Fatalf("already resolved changed: %+v %v", after, err)
	}
}
