package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RecheckIdentityCannotClearMissingQuota(t *testing.T) {
	for _, pruned := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "receipt"}[pruned], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if pruned {
				if _, err := store.PruneEventsBefore(event.Timestamp.Add(time.Second), 100); err != nil {
					t.Fatal(err)
				}
			}
			original, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				if err := putUnresolvedUsage(tx, event, original); err != nil {
					return err
				}
				return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
			}); err != nil {
				t.Fatal(err)
			}
			entries, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(entries) != 1 {
				t.Fatalf("evidence: %v %v", entries, err)
			}
			req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddd11", Actor: "admin"}
			result, err := store.RecheckUsage(req)
			if err != nil || result.Status != "identity_verified_accounting_unresolved" || result.AccountingVerified || result.AccountingRestored {
				t.Fatalf("identity must not clear accounting: %+v %v", result, err)
			}
			after, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(after) != 1 || after[0].Resolved || after[0].Version != entries[0].Version {
				t.Fatalf("evidence changed: %+v %v", after, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("missing quota shown as zero: %v %v", units, err)
			}
			if totals, err := store.SumQuotaByClientRange(event.ClientName, from, to); len(totals) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("missing quota shown complete: %v %v", totals, err)
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
			store = clockedStore(db)
			again, err := store.RecheckUsage(req)
			if err != nil || again != result {
				t.Fatalf("restart retry: %+v %v", again, err)
			}
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("restart lost block: %v", err)
			}
		})
	}
}

func TestIssue72IdentityOnlyRetryCannotClearMissingQuota(t *testing.T) {
	for _, entry := range []string{"record_retry", "late_registration"} {
		t.Run(entry, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			original, _ := json.Marshal(event)
			if err := db.Update(func(tx *bolt.Tx) error {
				if entry == "record_retry" {
					if err := putUnresolvedUsage(tx, event, original); err != nil {
						return err
					}
				}
				return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
			}); err != nil {
				t.Fatal(err)
			}
			if entry == "record_retry" {
				if added, err := store.RecordOnce(event); added || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Fatalf("identity retry: %v %v", added, err)
				}
			} else if err := store.RegisterUnresolvedUsage(event, original); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("identity cleared incomplete accounting: %v %v", units, err)
			}
		})
	}
}

func TestIssue72EqualRollupCannotProveReplacedRawIdentity(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa201"
	second.Timestamp = first.Timestamp.Add(time.Minute)
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(second); err != nil {
		t.Fatal(err)
	}
	replacement := second
	replacement.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa202"
	replacement.Timestamp = second.Timestamp.Add(time.Minute)
	original, _ := json.Marshal(first)
	replacementRaw, _ := json.Marshal(replacement)
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketUsageEvents))
		if err := bucket.Delete([]byte(usageKey(second.Timestamp, second.RecordID))); err != nil {
			return err
		}
		if err := bucket.Put([]byte(usageKey(replacement.Timestamp, replacement.RecordID)), replacementRaw); err != nil {
			return err
		}
		return putUnresolvedUsage(tx, first, original)
	}); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(first); added || err != nil {
		t.Fatalf("known identity retry: added=%v err=%v", added, err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("equal totals hid replaced raw identity: %v", err)
	}
}

func TestIssue72LegacyRollupWithoutProvenanceCannotResolveEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte("usage_accounting_proof_tracked_v1")); err != nil {
			return err
		}
		return putUnresolvedUsage(tx, event, original)
	}); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(event); added || err != nil {
		t.Fatalf("identity retry: %v %v", added, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("legacy raw and aggregate guessed complete: %v", err)
	}
}

func TestIssue72RecheckingResolvedEvidenceCannotKeepMissingQuotaUnblocked(t *testing.T) {
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
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddd15", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || result.Status != usageRecheckIdentityVerified {
		t.Fatalf("recheck: %+v %v", result, err)
	}
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 1 || !after[0].Resolved || after[0].Version != entries[0].Version {
		t.Fatalf("recheck changed previously resolved state: %+v %v", after, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing quota not blocked: %v", err)
	}
}

func TestIssue72RestartRechecksPreviouslyAutoResolvedAccounting(t *testing.T) {
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
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(unresolvedCanonicalKeys)); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
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
	store = clockedStore(db)
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 1 || after[0].Resolved {
		t.Fatalf("prior identity-only resolution survived missing accounting: %+v %v", after, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("restart hid incomplete accounting: %v", err)
	}
}

func TestIssue72OldRecheckReceiptCannotKeepAccountingUnblocked(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
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
		t.Fatalf("list: %v %v", entries, err)
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddd12", Actor: "admin"}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(unresolvedBucket))
		c := b.Cursor()
		key, raw := c.First()
		var item unresolvedUsage
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		item.Resolved = true
		updated, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if err := b.Put(key, updated); err != nil {
			return err
		}
		ops, err := tx.CreateBucket([]byte(usageRecheckBucket))
		if err != nil {
			return err
		}
		receipt, err := json.Marshal(usageRecheckReceipt{Request: req, Result: UsageRecheckResult{Status: "verified"}})
		if err != nil {
			return err
		}
		if err := ops.Put([]byte(req.OperationID), receipt); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(usageRecheckMarker), []byte("1")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageRecheckSafeMigration)); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
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
	store = clockedStore(db)
	after, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(after) != 1 || after[0].Resolved {
		t.Fatalf("old verified evidence not reactivated: %+v %v", after, err)
	}
	result, err := store.RecheckUsage(req)
	if err != nil || result.Status != "identity_verified_accounting_unresolved" {
		t.Fatalf("old retry claims accounting restoration: %+v %v", result, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("legacy recheck hid missing quota: %v", err)
	}
}

func TestIssue72UnattributedOldRecheckBlocksActivation(t *testing.T) {
	db := testDB(t)
	req := UsageRecheckRequest{EvidenceID: strings.Repeat("a", 64), Version: strings.Repeat("b", 64), OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddd16", Actor: "admin"}
	if err := db.Update(func(tx *bolt.Tx) error {
		ops, err := tx.CreateBucket([]byte(usageRecheckBucket))
		if err != nil {
			return err
		}
		value, err := json.Marshal(usageRecheckReceipt{Request: req, Result: UsageRecheckResult{Status: "verified"}})
		if err != nil {
			return err
		}
		if err := ops.Put([]byte(req.OperationID), value); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Put([]byte(usageRecheckMarker), []byte("1")); err != nil {
			return err
		}
		return meta.Delete([]byte(usageRecheckSafeMigration))
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	from, to := boundScanTime, boundScanTime.Add(time.Hour)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("unattributed legacy recheck hidden: %v", err)
	}
}

func TestIssue72BackfillRetainsKnownRawScope(t *testing.T) {
	for _, tc := range []struct {
		name, owner    string
		at             time.Time
		unrelatedOwner string
		unrelatedAt    time.Time
	}{
		{"known_client_unknown_time", "alice", time.Time{}, "bob", boundScanTime},
		{"known_time_unknown_client", "", boundScanTime, "bob", boundScanTime.Add(48 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			event := fixedPlainEvent()
			event.ClientName, event.Timestamp = tc.owner, tc.at
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("legacy-fixture"), raw)
			}); err != nil {
				t.Fatal(err)
			}
			if err := BackfillQuotaUsageAgg(db); err != nil {
				t.Fatal(err)
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
			store := clockedStore(db)
			found := false
			if err := db.View(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(unresolvedBucket)).ForEach(func(_, data []byte) error {
					var item unresolvedUsage
					if err := json.Unmarshal(data, &item); err != nil {
						return err
					}
					var evidence backfillEvidence
					if item.Kind == "aggregate_backfill" && json.Unmarshal(item.Original, &evidence) == nil && bytes.Equal(evidence.Value, raw) {
						found = true
					}
					return nil
				})
			}); err != nil || !found {
				t.Fatalf("original raw evidence was not preserved: found=%v err=%v", found, err)
			}
			from, to := tc.unrelatedAt.Truncate(time.Hour), tc.unrelatedAt.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: tc.unrelatedOwner, From: &from, To: &to}, replayCosts, false); err != nil {
				t.Fatalf("known disjoint raw scope blocked: %v", err)
			}
			owner, at := tc.owner, tc.at
			if owner == "" {
				owner = "bob"
			}
			if at.IsZero() {
				at = boundScanTime
			}
			from, to = at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: owner, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("relevant raw evidence did not block: %v", err)
			}
		})
	}
}

func TestIssue72MalformedHourFiltersConsistently(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	hour := boundScanTime.Truncate(time.Hour)
	putQuotaAggRaw(t, db, hour.Format(hourLayout)+"x|alice|target|model-a", []byte(`{"requests":1}`))
	from, to := hour, hour.Add(time.Hour-time.Nanosecond)
	for _, resolver := range []usage.RateResolver{replayCosts, exactCostTable{}} {
		for _, tc := range []struct {
			client, model string
			blocked       bool
		}{{"bob", "", false}, {"alice", "model-b", false}, {"alice", "model-a", true}, {"", "model-a", true}} {
			filter := usage.Filter{ClientName: tc.client, Model: tc.model, From: &from, To: &to}
			units, err := store.ReadCostUnits(context.Background(), filter, resolver, false)
			if tc.blocked && (!errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0) {
				t.Errorf("affected %+v: %v %v", tc, units, err)
			}
			if !tc.blocked && err != nil {
				t.Errorf("unrelated %+v: %v", tc, err)
			}
		}
	}
}
