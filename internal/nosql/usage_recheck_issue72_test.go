package nosql

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RecheckRequiresMatchingEvidenceAndIsIdempotent(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || result.Status != usageRecheckAccountingVerified || !result.AccountingVerified || !result.AccountingRestored {
		t.Fatalf("recheck: %+v %v", result, err)
	}
	changed, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(changed) != 1 || !changed[0].Resolved || changed[0].Version == entries[0].Version {
		t.Fatalf("proven recheck did not restore evidence: %+v %v", changed, err)
	}
	again, err := store.RecheckUsage(req)
	if err != nil || again != result {
		t.Fatalf("retry: %+v %v", again, err)
	}
	req.Version = "different"
	if _, err := store.RecheckUsage(req); err == nil {
		t.Fatal("conflicting operation accepted")
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	req.Version = entries[0].Version
	again, err = NewUsageStore(db).RecheckUsage(req)
	if err != nil || again != result {
		t.Fatalf("restart retry: %+v %v", again, err)
	}
}

func TestIssue72RecheckVersionConflictHasIdempotentReceipt(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: strings.Repeat("0", 64), OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Actor: "admin"}
	for i := 0; i < 2; i++ {
		if _, err := store.RecheckUsage(req); !errors.Is(err, ErrUsageRecheckConflict) {
			t.Fatalf("version conflict: %v", err)
		}
	}
	req.Version = entries[0].Version
	if _, err := store.RecheckUsage(req); !errors.Is(err, ErrUsageRecheckConflict) {
		t.Fatalf("reused operation: %v", err)
	}
	events, err := NewAuditStore(db).List(10)
	if err != nil || len(events) != 3 {
		t.Fatalf("rejections not audited: %+v %v", events, err)
	}
}

func TestIssue72RecheckRejectsConflictAndAuditFailure(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	changed := usage.CloneEvent(event)
	changed.InputTokens++
	original, _ := json.Marshal(changed)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, changed, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || result.Status != "insufficient_evidence" {
		t.Fatalf("conflict: %+v %v", result, err)
	}
	entries, _, err = store.ListUsageRechecks(0, 100)
	if err != nil || entries[0].Resolved {
		t.Fatalf("conflict resolved: %+v %v", entries, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketAdminAudit)) }); err != nil {
		t.Fatal(err)
	}
	req.OperationID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := store.RecheckUsage(req); err == nil {
		t.Fatal("missing audit accepted")
	}
}

func TestIssue72RecheckRejectsIncompleteBundles(t *testing.T) {
	for _, scenario := range []string{"missing", "partial", "conflict", "unknown_owner", "aggregate", "mismatched_original", "previously_resolved", "supplementary"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedIdentityBundle()
			if scenario != "missing" && scenario != "unknown_owner" && scenario != "aggregate" {
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "partial" {
				deleteUsageRawAndIndex(t, db, usage.ExpandEvent(event)[1])
			}
			if scenario == "supplementary" {
				event = usage.ExpandEvent(event)[1]
			}
			if scenario == "conflict" || scenario == "previously_resolved" {
				event.InputTokens++
			}
			if scenario == "unknown_owner" {
				event.ClientName = ""
			}
			original, _ := json.Marshal(event)
			if scenario == "mismatched_original" {
				original = []byte(`{"record_id":"wrong"}`)
			}
			item := unresolvedUsage{Event: event, Original: original, Resolved: scenario == "previously_resolved"}
			if scenario == "aggregate" {
				item.Kind = "aggregate_backfill"
			}
			if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedEvidence(tx, item) }); err != nil {
				t.Fatal(err)
			}
			entries, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(entries) != 1 {
				t.Fatalf("list: %v %v", entries, err)
			}
			req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", Actor: "admin"}
			result, err := store.RecheckUsage(req)
			if err != nil || result.Status != "insufficient_evidence" {
				t.Fatalf("%s: %+v %v", scenario, result, err)
			}
			after, _, err := store.ListUsageRechecks(0, 100)
			if scenario == "previously_resolved" {
				if err != nil || !after[0].Resolved || after[0].Version != entries[0].Version {
					t.Fatalf("previously proven conflict changed: %+v %v", after, err)
				}
			} else if err != nil || after[0].Version != entries[0].Version || after[0].Resolved != item.Resolved {
				t.Fatalf("state changed: %+v %v", after, err)
			}
		})
	}
}

func TestIssue72RecheckAuditFailureRollsBackSuccessfulVerification(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketAdminAudit)) }); err != nil {
		t.Fatal(err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Actor: "admin"}
	if _, err := store.RecheckUsage(req); err == nil {
		t.Fatal("missing audit accepted")
	}
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || after[0].Version != entries[0].Version || after[0].Resolved {
		t.Fatalf("state changed: %+v %v", after, err)
	}
}

func TestIssue72RecheckLateAuditFailureRollsBack(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := store.ListUsageRechecks(0, 100)
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "11111111-1111-4111-8111-111111111111", Actor: "admin"}
	if _, err := store.recheckUsage(req, func(*bolt.Tx, AuditEvent) error { return errors.New("audit write failed") }); err == nil {
		t.Fatal("audit failure ignored")
	}
	after, _, _ := store.ListUsageRechecks(0, 100)
	if after[0].Version != entries[0].Version || after[0].Resolved {
		t.Fatal("state changed despite audit failure")
	}
	result, err := store.RecheckUsage(req)
	if err != nil || result.Status != usageRecheckAccountingVerified || !result.AccountingVerified || !result.AccountingRestored {
		t.Fatalf("receipt survived rollback: %+v %v", result, err)
	}
}

func TestIssue72RecheckReceiptAndRetainedRollupProof(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "22222222-2222-4222-8222-222222222222", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || result.Status != usageRecheckAccountingVerified || !result.AccountingVerified || !result.AccountingRestored {
		t.Fatalf("matching receipts: %+v %v", result, err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageEvents)).Stats().KeyN != 0 {
			t.Error("raw rows unexpectedly present")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue72RecheckConcurrentRetryAndSingleEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(event)
	second := append([]byte(" "), first...)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := putUnresolvedUsage(tx, event, first); err != nil {
			return err
		}
		return putUnresolvedUsage(tx, event, second)
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 2 {
		t.Fatalf("list: %+v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "33333333-3333-4333-8333-333333333333", Actor: "admin"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := store.RecheckUsage(req)
			if err != nil || result.Status != usageRecheckAccountingVerified || !result.AccountingVerified || !result.AccountingRestored {
				t.Errorf("retry: %+v %v", result, err)
			}
		}()
	}
	wg.Wait()
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 2 || !after[0].Resolved || after[1].Resolved || after[0].Version == entries[0].Version || after[1].Version != entries[1].Version {
		t.Fatalf("retry changed wrong evidence: %+v %v", after, err)
	}
}
