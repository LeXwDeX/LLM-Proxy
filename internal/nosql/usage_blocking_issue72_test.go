package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72PrecheckFailurePreservesCurrentEvent(t *testing.T) {
	for _, kind := range []string{"unreadable", "corrupt", "pending_conflict"} {
		t.Run(kind, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			path := filepath.Join(t.TempDir(), "usage.journal")
			journal, err := NewUsageJournal(store, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unreadable":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				mustWriteFile(t, path, "{bad}\n")
			case "pending_conflict":
				other := event
				other.InputTokens++
				mustWriteFile(t, path, journalLine(t, other)+"\n")
			}
			if added, err := journal.RecordOnce(event); added || err == nil {
				t.Fatalf("precheck: added=%v err=%v", added, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("current event lost after %s: %v %v", kind, units, err)
			}
		})
	}
}

func TestIssue72PrecheckFailureFallsBackToJournal(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(unresolvedInitialized), []byte("invalid"))
	}); err != nil {
		t.Fatal(err)
	}
	event := fixedPlainEvent()
	other := event
	other.InputTokens++
	path := filepath.Join(t.TempDir(), "usage.journal")
	mustWriteFile(t, path, journalLine(t, other)+"\n")
	journal, err := NewUsageJournal(store, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := journal.RecordOnce(event); added || err == nil {
		t.Fatalf("precheck: added=%v err=%v", added, err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != journalLine(t, other)+"\n"+journalLine(t, event)+"\n" {
		t.Fatalf("pending evidence lost: %s", contents)
	}
}

func TestIssue72LateEvidenceRegistrationDoesNotBlockCommittedRetry(t *testing.T) {
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
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("late evidence hid committed event: %v %v", units, err)
	}
}

func TestIssue72LateVerifiedDigestConflictDoesNotBlock(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	conflict := event
	conflict.InputTokens++
	original, _ := json.Marshal(conflict)
	if err := store.RegisterUnresolvedUsage(conflict, original); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("verified conflict blocked accounting: %v %v", units, err)
	}
}

func TestIssue72PendingDigestConflictResolvesOnlyAfterWholeBundleCommits(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	changed := usage.CloneEvent(event)
	changed.Components[0].InputTokens++
	original, _ := json.Marshal(changed)
	if err := store.RegisterUnresolvedUsage(changed, original); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("uncommitted conflict did not block: %v", err)
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Fatalf("verified complete conflict remained unresolved: %v", err)
	}
	found := 0
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(unresolvedBucket)).ForEach(func(_, value []byte) error {
			found++
			var item unresolvedUsage
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			if string(item.Original) != string(original) || !item.Resolved {
				t.Errorf("conflict evidence changed or not resolved: %+v", item)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("conflict evidence count=%d, want 1", found)
	}
}

func TestIssue72PendingConflictWithExtraComponentStaysUnresolved(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	changed := usage.CloneEvent(event)
	changed.Components = append(changed.Components, usage.Component{Model: "extra", IterationIndex: 999, InputTokens: 3})
	original, _ := json.Marshal(changed)
	if err := store.RegisterUnresolvedUsage(changed, original); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("uncommitted component was resolved as conflict: %v", err)
	}
}

func TestIssue72UnnormalizedEvidenceResolvesAfterCommit(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName = " alice "
	event.EndpointType = "CLAUDE"
	event.Model = "EXECUTOR"
	event.Target = " anthropic "
	original, _ := json.Marshal(event)
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(event); !added || err != nil {
		t.Fatalf("record: added=%v err=%v", added, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("retry did not resolve normalized evidence: %v %v", units, err)
	}
}

func TestIssue72LegacyUnnormalizedEvidenceResolvesAfterCommit(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName, event.Model, event.Target = " alice ", "EXECUTOR", " anthropic "
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	if added, err := store.RecordOnce(event); !added || err != nil {
		t.Fatalf("record: added=%v err=%v", added, err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("legacy evidence did not resolve: %v %v", units, err)
	}
}

func TestIssue72UnresolvedBundleHonorsModelAndGroupScope(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.Target, event.Model = "target-a", "root"
	event.Components = []usage.Component{{Model: "advisor", IterationIndex: 1}}
	if err := store.RegisterUnresolvedUsage(event, []byte("bundle raw")); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	for _, model := range []string{"root", "advisor", "unrelated"} {
		filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to, Model: model}
		_, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
		if model == "unrelated" && err != nil {
			t.Errorf("unrelated model blocked: %v", err)
		}
		if model != "unrelated" && !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("model %s missed unresolved bundle: %v", model, err)
		}
		_, err = store.Aggregate(filter, "hour", replayCosts)
		if model == "unrelated" && err != nil {
			t.Errorf("unrelated aggregate blocked: %v", err)
		}
		if model != "unrelated" && !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("aggregate %s missed unresolved bundle: %v", model, err)
		}
		_, err = store.SumTargetModelRange(filter)
		if model == "unrelated" && err != nil {
			t.Errorf("unrelated target/model blocked: %v", err)
		}
		if model != "unrelated" && !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Errorf("target/model %s missed unresolved bundle: %v", model, err)
		}
	}
	targets, groups := NewTargetStore(db), NewModelGroupStore(db)
	if err := targets.Create(config.Target{Name: "target-a", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "root"}, {Upstream: "advisor"}}}); err != nil {
		t.Fatal(err)
	}
	if err := groups.Create(config.ModelGroup{Name: "advisor-group", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "target-a", Model: "advisor"}}}); err != nil {
		t.Fatal(err)
	}
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to, ModelGroups: []string{"unrelated"}}
	if _, err := store.AggregateModelGroups(filter, replayCosts, targets, groups); err != nil {
		t.Errorf("unrelated group blocked: %v", err)
	}
	filter.ModelGroups = []string{"advisor-group"}
	if _, err := store.AggregateModelGroups(filter, replayCosts, targets, groups); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("component group not blocked: %v", err)
	}
	filter.Model = "unrelated"
	if _, err := store.AggregateModelGroups(filter, replayCosts, targets, groups); err != nil {
		t.Errorf("unrelated model in component group blocked: %v", err)
	}
}

func TestIssue72UnknownUnresolvedModelDoesNotGuessScope(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.Model = ""
	if err := store.RegisterUnresolvedUsage(event, []byte("unknown model")); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, Model: "any", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown model treated as unrelated: %v", err)
	}
}

func TestIssue72CalibrationCanScopeHistoricalEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.RegisterUnresolvedUsage(event, []byte("old evidence")); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	drain, ok := any(journal).(interface {
		DrainClientRange(string, time.Time, time.Time) error
	})
	if !ok {
		t.Fatal("scoped calibration drain is unavailable")
	}
	from, to := event.Timestamp.Add(48*time.Hour), event.Timestamp.Add(49*time.Hour)
	if err := drain.DrainClientRange(event.ClientName, from, to); err != nil {
		t.Fatalf("old resolved-range evidence blocked drain: %v", err)
	}
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Fatalf("out-of-range event blocked calibration: %v", err)
	}
	from, to = event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if err := drain.DrainClientRange(event.ClientName, from, to); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("in-range evidence did not block drain: %v", err)
	}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("in-range event did not block calibration: %v", err)
	}
}

func TestIssue72CalibrationScopedReadStillRejectsUnknownEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.Timestamp = time.Time{}
	if err := store.RegisterUnresolvedUsage(event, []byte("unknown timestamp")); err != nil {
		t.Fatal(err)
	}
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage.journal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	from, to := boundScanTime, boundScanTime.Add(time.Hour)
	drain, ok := any(journal).(interface {
		DrainClientRange(string, time.Time, time.Time) error
	})
	if !ok {
		t.Fatal("scoped calibration drain is unavailable")
	}
	if err := drain.DrainClientRange(event.ClientName, from, to); !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("unknown evidence did not block drain: %v", err)
	}
	_, err = store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if !errors.Is(err, ErrUsageAggregateSourceMissing) || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown evidence did not block scoped calibration: %v", err)
	}
}
