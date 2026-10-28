package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72RemovedComponentCannotResolveCommittedBundle(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(bundle.Timestamp, bundle.ClientName, bundle.Target, bundle.Components[0].Model))
	}); err != nil {
		t.Fatal(err)
	}
	candidate := usage.CloneEvent(bundle)
	candidate.Components = nil
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(candidate); added || !errors.Is(err, ErrUsageRecordConflict) {
		t.Fatalf("conflict: %v %v", added, err)
	}
	from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
	for _, model := range []string{"", "executor", "advisor"} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, Model: model, From: &from, To: &to}, replayCosts, false)
		if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("model %q missed incomplete bundle: %v %v", model, units, err)
		}
	}
	if quota, err := store.SumQuotaByClientRange(bundle.ClientName, from, to); len(quota) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("quota omitted component: %v %v", quota, err)
	}
	targets, groups := NewTargetStore(db), NewModelGroupStore(db)
	if err := targets.Create(config.Target{Name: bundle.Target, Endpoint: "https://example.com", APIKey: "test-key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "executor"}, {Upstream: "advisor"}}}); err != nil {
		t.Fatal(err)
	}
	if err := groups.Create(config.ModelGroup{Name: "advisor-group", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: bundle.Target, Model: "advisor"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AggregateModelGroups(usage.Filter{ClientName: bundle.ClientName, From: &from, To: &to, ModelGroups: []string{"advisor-group"}}, replayCosts, targets, groups); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("component group omitted: %v", err)
	}
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("unrelated client blocked: %v", err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) == 0 {
		t.Fatalf("evidence: %v %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Resolved {
			t.Fatalf("unproven bundle was resolved: %+v", entry)
		}
	}
	req := UsageRecheckRequest{EvidenceID: entries[0].ID, Version: entries[0].Version, OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa704", Actor: "admin"}
	result, err := store.RecheckUsage(req)
	if err != nil || result.AccountingVerified || result.AccountingRestored {
		t.Fatalf("recheck certified incomplete original bundle: %+v %v", result, err)
	}
	outFrom, outTo := from.Add(48*time.Hour), to.Add(48*time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, From: &outFrom, To: &outTo}, replayCosts, false); err != nil {
		t.Fatalf("unrelated time blocked: %v", err)
	}
	pathDB := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(pathDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, Model: "advisor", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("restart lost component scope: %v", err)
	}
}

func TestIssue72RemovedComponentConflictWithCompleteBundleIsSafe(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bundle := fixedIdentityBundle()
	if err := store.Record(bundle); err != nil {
		t.Fatal(err)
	}
	candidate := usage.CloneEvent(bundle)
	candidate.Components = nil
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(candidate); added || !errors.Is(err, ErrUsageRecordConflict) {
		t.Fatalf("conflict: %v %v", added, err)
	}
	from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 12 {
		t.Fatalf("complete bundle changed: %v %v", units, err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 || !entries[0].Resolved {
		t.Fatalf("complete conflict not proven: %+v %v", entries, err)
	}
}

func TestIssue72DifferentModelPruneRetainsEachIdentity(t *testing.T) {
	for _, batchSize := range []int{1, 100} {
		for _, damage := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch_%d_damage_%t", batchSize, damage), func(t *testing.T) {
				db := testDB(t)
				store := clockedStore(db)
				first := fixedPlainEvent()
				first.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa701"
				second := first
				second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaa702"
				second.Timestamp = first.Timestamp.Add(time.Minute)
				second.Model = "advisor"
				if err := store.Record(first); err != nil {
					t.Fatal(err)
				}
				if err := store.Record(second); err != nil {
					t.Fatal(err)
				}
				if damage {
					if err := db.Update(func(tx *bolt.Tx) error {
						return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(second.Timestamp, second.ClientName, second.Target, second.Model))
					}); err != nil {
						t.Fatal(err)
					}
				}
				if n, err := store.PruneEventsBefore(second.Timestamp.Add(2*time.Second), batchSize); err != nil || n != 2 {
					t.Fatalf("prune: %d %v", n, err)
				}
				pathDB := db.Path()
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err := OpenDB(pathDB)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = clockedStore(db)
				journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = journal.Close() })
				for i, event := range []usage.Event{first, second} {
					added, err := journal.RecordOnce(event)
					if added {
						t.Fatalf("retry %d was inserted", i)
					}
					if damage {
						if !errors.Is(err, ErrUsageHistoryIncomplete) {
							t.Errorf("client-hour retry %d ignored damaged accounting: %v", i, err)
						}
					} else if err != nil {
						t.Errorf("healthy retry %d blocked: %v", i, err)
					}
				}
				from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Add(time.Hour)
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false)
				if damage && (len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete)) {
					t.Fatalf("damaged total accepted: %v %v", units, err)
				}
				if !damage && (err != nil || costUnitTotal(units) != 7) {
					t.Fatalf("stable total: %v %v", units, err)
				}
				if damage {
					valid, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, Model: first.Model, From: &from, To: &to}, replayCosts, false)
					if err != nil || costUnitTotal(valid) != 2 {
						t.Fatalf("unrelated proven cell blocked: %v %v", valid, err)
					}
				}
				for i, event := range []usage.Event{first, second} {
					var req UsageRecheckRequest
					if err := db.View(func(tx *bolt.Tx) error {
						return tx.Bucket([]byte(unresolvedBucket)).ForEach(func(k, v []byte) error {
							var item unresolvedUsage
							if err := json.Unmarshal(v, &item); err != nil {
								return err
							}
							if item.Event.RecordID == event.RecordID {
								req = UsageRecheckRequest{EvidenceID: recheckHash(k), Version: recheckHash(v), OperationID: fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", 0xa703+i), Actor: "admin"}
							}
							return nil
						})
					}); err != nil {
						t.Fatal(err)
					}
					if req.EvidenceID == "" {
						t.Fatalf("identity %d missing evidence", i)
					}
					result, err := store.RecheckUsage(req)
					if err != nil {
						t.Fatal(err)
					}
					if damage && i == 1 {
						if result.AccountingVerified || result.AccountingRestored {
							t.Errorf("damaged recheck accepted: %+v", result)
						}
					} else if !result.AccountingVerified {
						t.Errorf("healthy recheck %d not verified: %+v", i, result)
					}
				}
			})
		}
	}
}

func TestIssue72UnprovenPruneImmediatelyBlocksAccounting(t *testing.T) {
	for _, invalidRaw := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidRaw), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)); err != nil {
					return err
				}
				if invalidRaw {
					return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, event.RecordID)), []byte("{broken"))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 1); err != nil || n != 1 {
				t.Fatalf("prune: %d %v", n, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("unproven prune reported complete: %v %v", units, err)
			}
			entries, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(entries) == 0 {
				t.Fatalf("unproven raw was not retained: %v %v", entries, err)
			}
		})
	}
}

func TestIssue72AmbiguousOriginalCannotAutoResolve(t *testing.T) {
	for _, scenario := range []string{"folded_duplicate", "case_variant", "nested_duplicate", "nested_case_variant", "unknown_field", "invalid_utf8"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedIdentityBundle()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(event)
			switch scenario {
			case "folded_duplicate":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"CLIENT_NAME":"bob"}`)
			case "case_variant":
				raw = []byte(strings.Replace(string(raw), `"client_name":`, `"CLIENT_NAME":`, 1))
			case "nested_duplicate":
				raw = []byte(strings.Replace(string(raw), `"iteration_index":`, `"iteration_index":999,"iteration_index":`, 1))
			case "nested_case_variant":
				raw = []byte(strings.Replace(string(raw), `"iteration_index":`, `"ITERATION_INDEX":`, 1))
			case "unknown_field":
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"future_field":1}`)
			case "invalid_utf8":
				raw = append([]byte(nil), raw...)
				raw = append(raw[:len(raw)-1], []byte(",\"future\":\"\xff\"}")...)
			}
			if err := store.RegisterUnresolvedUsage(event, raw); err != nil {
				t.Fatal(err)
			}
			entries, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(entries) != 1 || entries[0].Resolved {
				t.Fatalf("ambiguous original auto-resolved: %+v %v", entries, err)
			}
			if _, err := store.RecordOnce(event); err != nil {
				t.Fatal(err)
			}
			entries, _, err = store.ListUsageRechecks(0, 100)
			if err != nil || entries[0].Resolved {
				t.Fatalf("retry auto-resolved ambiguous original: %+v %v", entries, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("ambiguous evidence skipped: %v", err)
			}
			if scenario == "folded_duplicate" {
				if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Fatalf("ambiguous original owner guessed: %v", err)
				}
				from, to = from.Add(48*time.Hour), to.Add(48*time.Hour)
				if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
					t.Fatalf("known unrelated time blocked: %v", err)
				}
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
				t.Fatalf("restart auto-resolved ambiguous original: %+v %v", after, err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				_, value := tx.Bucket([]byte(unresolvedBucket)).Cursor().First()
				var item unresolvedUsage
				if err := json.Unmarshal(value, &item); err != nil {
					return err
				}
				if string(item.Original) != string(raw) {
					t.Error("restart changed ambiguous original bytes")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssue72OldResolvedAmbiguousOriginalIsReopenedOnUpgrade(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(event)
	raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"future_field":1}`)
	item := unresolvedUsage{Event: event, Original: raw, Resolved: true}
	key, value, err := unresolvedEvidenceKey(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte(unresolvedBucket))
		if err != nil {
			return err
		}
		if err := b.Put(key, value); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Put([]byte(unresolvedInitialized), []byte("1")); err != nil {
			return err
		}
		if err := meta.Put([]byte("usage_unresolved_events_canonical_v8"), []byte("1")); err != nil {
			return err
		}
		return meta.Delete([]byte(unresolvedCanonicalKeys))
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
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 || entries[0].Resolved {
		t.Fatalf("old ambiguous proof survived upgrade: %+v %v", entries, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("old ambiguous evidence not blocked: %v", err)
	}
}
