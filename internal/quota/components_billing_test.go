package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
)

func bundleManager(t *testing.T, coefficient float64) *Manager {
	t.Helper()
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "models", CostCoefficient: coefficient, DailyUSD: 100,
		Bindings: []config.QuotaGroupBinding{{Target: "anthropic"}}})
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{
			{Target: "anthropic", Model: "executor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
			{Target: "anthropic", Model: "advisor", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}},
		}, nil
	}, nil)
	m.Evaluate("alice")
	return m
}

func quotaBundle() usage.Event {
	return usage.Event{RecordID: uuid.NewString(), Timestamp: time.Now().UTC(), ClientName: "alice", Target: "anthropic",
		Model: "executor", InputTokens: 1_000_000, StatusCode: 200, Components: []usage.Component{
			{Model: "advisor", IterationType: "advisor", IterationIndex: 0, InputTokens: 2_000_000},
			{Model: "advisor", IterationType: "advisor", IterationIndex: 2, InputTokens: 3_000_000, NonBillable: true},
		}}
}

func bundleUsed(m *Manager) float64 {
	return m.Status("alice").Groups["models"].Quotas[DimensionDaily].Used
}

func TestRepeatedRecordIDDoesNotDoubleChargeMemory(t *testing.T) {
	m := bundleManager(t, 1)
	event := quotaBundle()
	event.Components = nil
	for i := 0; i < 2; i++ {
		if err := m.RecordUsage(m.usageStore, event); err != nil {
			t.Fatal(err)
		}
	}
	if got := bundleUsed(m); got != 2 {
		t.Fatalf("idempotent DB replay charged live counters %v, want 2", got)
	}
}

func TestBundleQuotaMatchesCalibrationAndDuplicateRetries(t *testing.T) {
	for _, coefficient := range []float64{1, .5, 0} {
		t.Run(fmt.Sprintf("coefficient_%g", coefficient), func(t *testing.T) {
			m := bundleManager(t, coefficient)
			event := quotaBundle()
			if err := m.RecordUsage(m.usageStore, event); err != nil {
				t.Fatal(err)
			}
			if got := bundleUsed(m); got != 12*coefficient {
				t.Fatalf("live usage=%v want=%v", got, 12*coefficient)
			}
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := m.RecordUsage(m.usageStore, event); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if got := bundleUsed(m); got != 12*coefficient {
				t.Fatalf("duplicate live charge=%v want=%v", got, 12*coefficient)
			}
			m.Evaluate("alice")
			if got := bundleUsed(m); got != 12*coefficient {
				t.Fatalf("calibrated usage=%v want=%v", got, 12*coefficient)
			}
			if err := m.RecordUsage(m.usageStore, event); err != nil {
				t.Fatal(err)
			}
			if got := bundleUsed(m); got != 12*coefficient {
				t.Fatalf("post-calibration retry charge=%v", got)
			}
			changed := usage.CloneEvent(event)
			changed.Components[0].InputTokens++
			if err := m.RecordUsage(m.usageStore, changed); err == nil {
				t.Fatal("changed bundle accepted under existing identity")
			}
			if got := bundleUsed(m); got != 12*coefficient {
				t.Fatalf("rejected bundle changed counters=%v", got)
			}
		})
	}
}

type bundleFaultRecorder struct {
	store          *nosql.UsageStore
	fail           atomic.Bool
	commitThenFail bool
}

func (r *bundleFaultRecorder) Record(event usage.Event) error {
	if r.fail.Load() && !r.commitThenFail {
		return errors.New("injected primary failure")
	}
	if err := r.store.Record(event); err != nil {
		return err
	}
	if r.fail.Load() {
		return errors.New("injected uncertain primary outcome")
	}
	return nil
}
func (r *bundleFaultRecorder) RecordBatch(events []usage.Event) error {
	if r.fail.Load() && !r.commitThenFail {
		return errors.New("injected primary batch failure")
	}
	if err := r.store.RecordBatch(events); err != nil {
		return err
	}
	if r.fail.Load() {
		return errors.New("injected uncertain batch outcome")
	}
	return nil
}
func (r *bundleFaultRecorder) RecordExists(event usage.Event) (bool, error) {
	return r.store.RecordExists(event)
}

func TestBundleJournalFailureReplayDrainAndRetryDoNotDoubleCharge(t *testing.T) {
	for _, scenario := range []struct{ async, uncertain bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("async_%t_uncertain_%t", scenario.async, scenario.uncertain), func(t *testing.T) {
			m := bundleManager(t, 1)
			primary := &bundleFaultRecorder{store: m.usageStore, commitThenFail: scenario.uncertain}
			primary.fail.Store(true)
			path := filepath.Join(t.TempDir(), "usage.jsonl")
			journal, err := nosql.NewUsageJournal(primary, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if scenario.async {
				if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{MaxBatch: 8}); err != nil {
					t.Fatal(err)
				}
			}
			m.usageJournal = journal
			event := quotaBundle()
			event.PricingContext = &usage.PricingContext{Version: 1, TotalInputTokens: 1_000_000, TotalInputKnown: true, TotalInputValid: true}
			event.Components[0].PricingContext = &usage.PricingContext{Version: 1, TotalInputTokens: 2_000_000, TotalInputKnown: true, TotalInputValid: true}
			original := usage.CloneEvent(event)
			if err := m.RecordUsage(journal, event); err != nil {
				t.Fatal(err)
			}
			event.PricingContext.TotalInputTokens = 1
			event.Components[0].InputTokens = 2
			event.Components[0].PricingContext.TotalInputTokens = 3
			journal.Flush()
			if got := bundleUsed(m); got != 12 {
				t.Fatalf("durably queued charge=%v", got)
			}
			if err := m.RecordUsage(journal, original); err != nil {
				t.Fatal(err)
			}
			if err := m.RecordUsage(journal, event); err == nil {
				t.Fatal("pending identity accepted changed component data")
			}
			if got := bundleUsed(m); got != 12 {
				t.Fatalf("pending duplicate charged again: %v", got)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(data, []byte{'\n'}) != 1 {
				t.Fatalf("one bundle must occupy one journal line: %q", data)
			}
			var saved usage.Event
			if err := json.Unmarshal(bytes.TrimSpace(data), &saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Components) != 2 || saved.Components[0].InputTokens != 2_000_000 || saved.Components[0].PricingContext.TotalInputTokens != 2_000_000 || saved.PricingContext.TotalInputTokens != 1_000_000 {
				t.Fatalf("journal lost isolated bundle: %+v", saved)
			}
			m.Evaluate("alice")
			if status := m.Status("alice"); status.CalibrationError == "" || bundleUsed(m) != 12 {
				t.Fatalf("failed drain replaced live accounting: %+v", status)
			}
			primary.fail.Store(false)
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				if _, err := journal.Replay(); err != nil {
					t.Error(err)
				}
			}()
			go func() { defer wg.Done(); m.Evaluate("alice") }()
			go func() {
				defer wg.Done()
				if err := m.RecordUsage(journal, original); err != nil {
					t.Error(err)
				}
			}()
			wg.Wait()
			m.Evaluate("alice")
			if status := m.Status("alice"); status.CalibrationError != "" || bundleUsed(m) != 12 {
				t.Fatalf("recovery/retry double charge or unavailable: %+v", status)
			}
			if err := m.RecordUsage(journal, original); err != nil {
				t.Fatal(err)
			}
			if got := bundleUsed(m); got != 12 {
				t.Fatalf("post-compaction retry charged again: %v", got)
			}
			rows, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
			if err != nil || len(rows) != 3 {
				t.Fatalf("recovered bundle rows=%d err=%v", len(rows), err)
			}
		})
	}
}

// A primary may commit before reporting an error. If the recovery journal also
// fails, a later idempotent retry must reconcile that previously uncharged bill.
func TestReviewBundleUncertainCommitAndJournalFailureRetry(t *testing.T) {
	m := bundleManager(t, 1)
	primary := &bundleFaultRecorder{store: m.usageStore, commitThenFail: true}
	primary.fail.Store(true)
	journal, err := nosql.NewUsageJournal(primary, filepath.Join(t.TempDir(), "nonexistent", "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	event := quotaBundle()
	if err := m.RecordUsage(journal, event); err == nil {
		t.Fatal("expected uncertain primary plus journal failure")
	}
	if got := bundleUsed(m); got != 0 {
		t.Fatalf("failed first call charged %v", got)
	}
	primary.fail.Store(false)
	if err := m.RecordUsage(journal, event); err != nil {
		t.Fatal(err)
	}
	if got := bundleUsed(m); got != 12 {
		t.Fatalf("durable retry not reflected in live quota: got %v, want 12", got)
	}
}

func TestBundlePersistenceFailureBlocksAdmissionAndRetryReconciles(t *testing.T) {
	for _, scenario := range []struct{ async, committed, explicitID bool }{
		{false, false, false}, {false, true, false}, {true, false, false}, {true, true, false},
		{false, false, true}, {false, true, true}, {true, false, true}, {true, true, true},
	} {
		t.Run(fmt.Sprintf("async_%t_committed_%t_id_%t", scenario.async, scenario.committed, scenario.explicitID), func(t *testing.T) {
			m := bundleManager(t, 1)
			primary := &bundleFaultRecorder{store: m.usageStore, commitThenFail: scenario.committed}
			primary.fail.Store(true)
			dir := filepath.Join(t.TempDir(), "unavailable")
			journal, err := nosql.NewUsageJournal(primary, filepath.Join(dir, "usage.jsonl"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if scenario.async {
				if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			m.usageJournal = journal
			event := quotaBundle()
			if !scenario.explicitID {
				event.RecordID = ""
			}
			if err := m.RecordUsage(journal, event); err == nil {
				t.Fatal("expected primary and journal failure")
			}
			if got := bundleUsed(m); got != 0 {
				t.Fatalf("unacknowledged write incremented memory: %v", got)
			}
			if status := m.Status("alice"); status.CalibrationError == "" {
				t.Fatal("uncertain write left accounting marked available")
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
				t.Fatalf("unsettled accounting admitted a request: %+v blocked=%t", info, blocked)
			}
			primary.fail.Store(false)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			want := 0.0
			if scenario.explicitID {
				// Every retry sees one stable request; calibration must wait for
				// any successful new increment and must not deadlock on its gate.
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if err := m.RecordUsage(journal, event); err != nil {
							t.Error(err)
						}
					}()
				}
				done := make(chan struct{})
				go func() { wg.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("retry reconciliation deadlocked")
				}
				want = 12
			} else {
				// No caller identity means there is no retry token. The existing
				// authoritative recovery path must discover only actual commits.
				m.Evaluate("alice")
				if scenario.committed {
					want = 12
				}
			}
			if status := m.Status("alice"); status.CalibrationError != "" || bundleUsed(m) != want {
				t.Fatalf("reconciliation: used=%v want=%v status=%+v", bundleUsed(m), want, status)
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
				t.Fatalf("healthy recovered accounting still blocked: %+v", info)
			}
			if scenario.explicitID {
				if err := m.RecordUsage(journal, event); err != nil {
					t.Fatal(err)
				}
				if got := bundleUsed(m); got != 12 {
					t.Fatalf("already reconciled retry charged again: %v", got)
				}
			}
		})
	}
}

func TestBundleAlreadyAccountedRetryAfterCalibrationErrorDoesNotDoubleCharge(t *testing.T) {
	m := bundleManager(t, 1)
	event := quotaBundle()
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	m.calibrationFailed("alice", errors.New("transient authoritative read failure"))
	if err := m.RecordUsage(m.usageStore, event); err != nil {
		t.Fatal(err)
	}
	if status := m.Status("alice"); status.CalibrationError != "" || bundleUsed(m) != 12 {
		t.Fatalf("accounted retry must replace rather than add: %+v", status)
	}
}
