package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72TamperedComponentCannotProveCommittedBundle(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	rows, keys, err := normalizeUsageBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		key := []byte(keys[1])
		if err := removeClientEventIndex(tx, key, b.Get(key)); err != nil {
			return err
		}
		child := rows[1]
		child.InputTokens = 1_000_000
		data, err := json.Marshal(child)
		if err != nil {
			return err
		}
		if err := b.Put(key, data); err != nil {
			return err
		}
		if err := addClientEventIndex(tx, child, key, data); err != nil {
			return err
		}
		var aggregate AggCell
		accumulateAggCell(&aggregate, child)
		cell, err := json.Marshal(aggregate)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey(child.Timestamp, child.EndpointType, child.ClientName, child.Model, child.Target), cell); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(quotaAggKey(child.Timestamp, child.ClientName, child.Target, child.Model), cell)
	}); err != nil {
		t.Fatal(err)
	}
	candidate := usage.CloneEvent(bundle)
	candidate.Components = nil
	if added, err := store.RecordOnce(candidate); added || !errors.Is(err, ErrUsageRecordConflict) {
		t.Fatalf("conflict: %v %v", added, err)
	}
	from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
	for _, model := range []string{"", "advisor"} {
		if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, Model: model, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("model %q: %v %v", model, units, err)
		}
	}
	if quota, err := store.SumQuotaByClientRange(bundle.ClientName, from, to); len(quota) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("quota: %v %v", quota, err)
	}
	targets, groups := NewTargetStore(db), NewModelGroupStore(db)
	if err := targets.Create(config.Target{Name: bundle.Target, Endpoint: "https://example.com", APIKey: "test-key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "advisor"}}}); err != nil {
		t.Fatal(err)
	}
	if err := groups.Create(config.ModelGroup{Name: "advisor-group", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: bundle.Target, Model: "advisor"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AggregateModelGroups(usage.Filter{ClientName: bundle.ClientName, From: &from, To: &to, ModelGroups: []string{"advisor-group"}}, replayCosts, targets, groups); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("model group: %v", err)
	}
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("unrelated client blocked: %v", err)
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
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, Model: "advisor", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("restart lost component conflict: %v", err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) == 0 {
		t.Fatalf("conflict evidence: %v %v", entries, err)
	}
	result, err := store.RecheckUsage(UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa705", Actor: "admin"})
	if err != nil || result.AccountingVerified || result.AccountingRestored {
		t.Fatalf("recheck accepted changed child: %+v %v", result, err)
	}
}

func TestIssue72ChildFirstPrunePreservesBundleProof(t *testing.T) {
	for _, batch := range []int{1, 100} {
		for _, damaged := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch_%d_damaged_%t", batch, damaged), func(t *testing.T) {
				db := testDB(t)
				store := clockedStore(db)
				bundle := fixedIdentityBundle()
				bundle.RecordID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
				rows, keys, err := normalizeUsageBundle(bundle)
				if err != nil || keys[1] >= keys[0] {
					t.Fatalf("child must precede root: %v %v", keys, err)
				}
				if err := store.Record(bundle); err != nil {
					t.Fatal(err)
				}
				if damaged {
					if err := db.Update(func(tx *bolt.Tx) error {
						return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(bundle.Timestamp, bundle.ClientName, bundle.Target, rows[1].Model))
					}); err != nil {
						t.Fatal(err)
					}
				}
				if n, err := store.PruneEventsBefore(bundle.Timestamp.Add(2*time.Second), batch); n != 2 || err != nil {
					t.Fatalf("prune: %d %v", n, err)
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
				journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "journal"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = journal.Close() })
				added, retryErr := journal.RecordOnce(bundle)
				from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
				units, readErr := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, From: &from, To: &to}, replayCosts, false)
				quota, quotaErr := store.SumQuotaByClientRange(bundle.ClientName, from, to)
				if damaged {
					if added || !errors.Is(retryErr, ErrUsageHistoryIncomplete) || len(units) != 0 || !errors.Is(readErr, ErrUsageHistoryIncomplete) || len(quota) != 0 || !errors.Is(quotaErr, ErrUsageHistoryIncomplete) {
						t.Fatalf("damaged bundle accepted: %v %v %v %v %v %v", added, retryErr, units, readErr, quota, quotaErr)
					}
				} else if added || retryErr != nil || readErr != nil || costUnitTotal(units) != 12 || quotaErr != nil || quota[bundle.Target+":advisor"].InputTokens != 2_000_000 {
					t.Fatalf("healthy bundle: %v %v %v %v %v %v", added, retryErr, units, readErr, quota, quotaErr)
				}
			})
		}
	}
}

func TestIssue72ChildFirstPruneRestartBetweenBatches(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	bundle.RecordID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	rows, keys, err := normalizeUsageBundle(bundle)
	if err != nil || keys[1] >= keys[0] {
		t.Fatalf("child order: %v %v", keys, err)
	}
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	// Commit just the child transaction to simulate a stop between batches.
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		key := []byte(keys[1])
		data := append([]byte(nil), raw.Get(key)...)
		if err := capturePruneRollupProof(tx, rows[1], key); err != nil {
			return err
		}
		if err := removeClientEventIndex(tx, key, data); err != nil {
			return err
		}
		digest, err := usage.RetryEquivalentDigest(rows[1])
		if err != nil {
			return err
		}
		if err := writeUsageReceipt(tx, key, digest); err != nil {
			return err
		}
		if err := writeReceiptOwner(tx, key, digest, rows[1], data); err != nil {
			return err
		}
		if err := raw.Delete(key); err != nil {
			return err
		}
		return bumpRawRevision(tx)
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
	if n, err := store.PruneEventsBefore(bundle.Timestamp.Add(2*time.Second), 1); n != 1 || err != nil {
		t.Fatalf("resume prune: %d %v", n, err)
	}
	if added, err := store.RecordOnce(bundle); added || err != nil {
		t.Fatalf("retry after restart: %v %v", added, err)
	}
	from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 12 {
		t.Fatalf("cost after restart: %v %v", units, err)
	}
}

func TestIssue72PruneMissingBundleChildDoesNotCertifyRoot(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	bundle.RecordID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	_, keys, err := normalizeUsageBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Delete([]byte(keys[1]))
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(bundle.Timestamp.Add(2*time.Second), 1); n != 1 || err != nil {
		t.Fatalf("prune: %d %v", n, err)
	}
	from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, Model: "advisor", From: &from, To: &to}, replayCosts, false)
	if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing child certified: %v %v", units, err)
	}
}
