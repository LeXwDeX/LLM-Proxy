package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72ConflictProtectsCommittedAndCandidateScopes(t *testing.T) {
	for _, change := range []string{"model", "target", "component"} {
		t.Run(change, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			original := fixedIdentityBundle()
			if err := store.Record(original); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(original.Timestamp, original.ClientName, original.Target, original.Model))
			}); err != nil {
				t.Fatal(err)
			}
			candidate := usage.CloneEvent(original)
			switch change {
			case "model":
				candidate.Model = "changed"
			case "target":
				candidate.Target = "target-b"
			case "component":
				candidate.Components[0].Model = "changed"
			}
			path := filepath.Join(t.TempDir(), "usage.journal")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if added, err := journal.RecordOnce(candidate); added || !errors.Is(err, ErrUsageRecordConflict) {
				t.Fatalf("conflict: %v %v", added, err)
			}
			from, to := original.Timestamp.Truncate(time.Hour), original.Timestamp.Add(time.Hour)
			for _, model := range []string{"executor", "advisor", "changed"} {
				filter := usage.Filter{ClientName: original.ClientName, Model: model, From: &from, To: &to}
				_, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
				relevant := model == "executor" || model == "advisor" || (model == "changed" && change != "target")
				if relevant && !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("%s scope %s not blocked: %v", change, model, err)
				}
				if !relevant && err != nil {
					t.Errorf("%s unrelated model %s blocked: %v", change, model, err)
				}
			}
			filter := usage.Filter{ClientName: "bob", From: &from, To: &to}
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
				t.Fatalf("unrelated client blocked: %v", err)
			}
			from, to = from.Add(48*time.Hour), to.Add(48*time.Hour)
			filter.ClientName = "alice"
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
				t.Fatalf("unrelated time blocked: %v", err)
			}
			entries, _, err := store.ListUsageRechecks(0, 100)
			if err != nil || len(entries) == 0 {
				t.Fatalf("missing evidence: %v %v", entries, err)
			}
			wantRaw, _ := json.Marshal(candidate)
			found := false
			if err := db.View(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(unresolvedBucket)).ForEach(func(_, raw []byte) error {
					var item unresolvedUsage
					if err := json.Unmarshal(raw, &item); err != nil {
						return err
					}
					if bytes.Equal(item.Original, wantRaw) {
						found = true
					}
					return nil
				})
			}); err != nil || !found {
				t.Fatalf("original conflicting evidence lost: found=%v err=%v", found, err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
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
			store = clockedStore(db)
			from, to = original.Timestamp.Truncate(time.Hour), original.Timestamp.Add(time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "executor", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("restart lost committed scope: %v", err)
			}
		})
	}
}

func TestIssue72ConflictProtectsBothModelGroups(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.Target = "target-a"
	event.Model = "old"
	targets, groups := NewTargetStore(db), NewModelGroupStore(db)
	for _, name := range []string{"target-a", "target-b"} {
		if err := targets.Create(config.Target{Name: name, Endpoint: "https://example.com", APIKey: "test-key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "old"}, {Upstream: "new"}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []struct{ name, target, model string }{{"old-group", "target-a", "old"}, {"new-group", "target-b", "new"}, {"other-group", "target-a", "new"}} {
		if err := groups.Create(config.ModelGroup{Name: v.name, CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: v.target, Model: v.model}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	candidate := event
	candidate.Target = "target-b"
	candidate.Model = "new"
	raw, _ := json.Marshal(candidate)
	if err := store.RegisterUnresolvedUsage(candidate, raw); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	for _, group := range []string{"old-group", "new-group", "other-group"} {
		filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to, ModelGroups: []string{group}}
		_, err := store.AggregateModelGroups(filter, replayCosts, targets, groups)
		if group != "other-group" && !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("group %s not blocked: %v", group, err)
		}
		if group == "other-group" && err != nil {
			t.Errorf("unrelated group blocked: %v", err)
		}
	}
}

func TestIssue72CrossClientConflictProtectsBothOwnersOnly(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	candidate := event
	candidate.ClientName = "bob"
	candidate.Model = "different"
	raw, _ := json.Marshal(candidate)
	if err := store.RegisterUnresolvedUsage(candidate, raw); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	for _, tc := range []struct {
		client, model string
		blocked       bool
	}{{"alice", "executor", true}, {"bob", "different", true}, {"alice", "different", false}, {"charlie", "executor", false}} {
		_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: tc.client, Model: tc.model, From: &from, To: &to}, replayCosts, false)
		if tc.blocked && !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("missing scope %+v: %v", tc, err)
		}
		if !tc.blocked && err != nil {
			t.Errorf("unrelated scope %+v blocked: %v", tc, err)
		}
	}
}

func TestIssue72ResolvedConflictRevalidatesCommittedScopeOnRead(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	candidate := event
	candidate.Model = "different"
	raw, _ := json.Marshal(candidate)
	if err := store.RegisterUnresolvedUsage(candidate, raw); err != nil {
		t.Fatal(err)
	}
	entries, _, err := store.ListUsageRechecks(0, 100)
	if err != nil || len(entries) != 1 || !entries[0].Resolved {
		t.Fatalf("fixture did not prove conflict: %+v %v", entries, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, Model: event.Model, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("stale conflict proof hid lost quota: %v", err)
	}
}

func TestIssue72PrunedConflictUsesOnlyProvenReceiptScope(t *testing.T) {
	for _, hasCells := range []bool{true, false} {
		t.Run(map[bool]string{true: "known_cells", false: "legacy_owner_only"}[hasCells], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 100); err != nil {
				t.Fatal(err)
			}
			if !hasCells {
				if err := db.Update(func(tx *bolt.Tx) error {
					b := tx.Bucket([]byte(receiptOwnersBucket))
					key := []byte(usageKey(event.Timestamp, event.RecordID))
					var item receiptOwner
					if err := json.Unmarshal(b.Get(key), &item); err != nil {
						return err
					}
					item.UsageKey = nil
					item.QuotaKey = nil
					item.CellBinding = ""
					data, err := json.Marshal(item)
					if err != nil {
						return err
					}
					return b.Put(key, data)
				}); err != nil {
					t.Fatal(err)
				}
			}
			candidate := event
			candidate.Model = "different"
			raw, _ := json.Marshal(candidate)
			if err := store.RegisterUnresolvedUsage(candidate, raw); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "executor", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("committed scope not blocked: %v", err)
			}
			_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "unrelated", From: &from, To: &to}, replayCosts, false)
			if hasCells && err != nil {
				t.Fatalf("proven unrelated model blocked: %v", err)
			}
			if !hasCells && !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("unknown legacy model guessed unrelated: %v", err)
			}
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
				t.Fatalf("known unrelated client blocked: %v", err)
			}
		})
	}
}

func TestIssue72OldConflictEvidenceGainsCommittedScopeOnRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	candidate := event
	candidate.Model = "different"
	original, _ := json.Marshal(candidate)
	item := unresolvedUsage{Event: candidate, Original: original}
	key, value, err := unresolvedEvidenceKey(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)); err != nil {
			return err
		}
		bucket, err := tx.CreateBucket([]byte(unresolvedBucket))
		if err != nil {
			return err
		}
		if err := bucket.Put(key, value); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Put([]byte(unresolvedInitialized), []byte("1")); err != nil {
			return err
		}
		if err := meta.Put([]byte("usage_unresolved_events_canonical_v5"), []byte("1")); err != nil {
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
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, Model: event.Model, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("old evidence lost committed scope: %v", err)
	}
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("unrelated owner blocked: %v", err)
	}
}
