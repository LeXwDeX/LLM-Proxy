package quota

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// gatedUsageStore holds the asynchronous batch commit until released; the
// synchronous Record path (calibration drain, replay) stays available.
type gatedUsageStore struct {
	store *nosql.UsageStore
	gate  chan struct{}
}

func (g *gatedUsageStore) Record(event usage.Event) error { return g.store.Record(event) }

func (g *gatedUsageStore) RecordBatch(events []usage.Event) error {
	<-g.gate
	return g.store.RecordBatch(events)
}

// With bbolt commits taken off the request path, the in-memory quota counter
// must still be charged before RecordUsage returns, so the very next request's
// admission sees the previous request's usage; calibration while the commit is
// pending drains the queue and charges the event exactly once.
func TestAsyncJournalNextAdmissionSeesPreviousUsageBeforeCommit(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	quotaTestCost(t, db, "qwen-max")
	m.Evaluate("alice")
	if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); blocked {
		t.Fatalf("fresh client blocked: %+v", info)
	}

	primary := &gatedUsageStore{store: m.usageStore, gate: make(chan struct{})}
	journal, err := nosql.NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal

	// First request: $10 against a $5 daily limit.
	recorded := make(chan error, 1)
	go func() {
		recorded <- m.RecordUsage(journal, usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream",
			EndpointType: "openai", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200})
	}()
	select {
	case err := <-recorded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecordUsage waited for the database commit")
	}
	if events, err := m.usageStore.List(usage.Filter{ClientName: "alice"}); err != nil || len(events) != 0 {
		t.Fatalf("commit should still be pending: events=%d err=%v", len(events), err)
	}
	// Second request's admission: the in-memory counter already holds $10.
	if info, blocked := m.CheckTarget("alice", "upstream", "qwen-max"); !blocked || info.Scope != ScopeModelGroup {
		t.Fatalf("next admission did not see the previous usage: %+v blocked=%v", info, blocked)
	}
	if used := m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used; used != 10 {
		t.Fatalf("in-memory charge = %v, want 10", used)
	}

	// Calibration started while the asynchronous commit is pending waits for
	// the writer's batch and charges the event exactly once.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(primary.gate)
	}()
	m.Evaluate("alice")
	status := m.Status("alice")
	if status.CalibrationError != "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
		t.Fatalf("calibration during pending commit: %+v", status)
	}

	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	m.Evaluate("alice")
	status = m.Status("alice")
	if status.CalibrationError != "" || status.Groups["domestic"].Quotas[DimensionDaily].Used != 10 {
		t.Fatalf("asynchronous commit after drain double counted: %+v", status)
	}
	if events, err := m.usageStore.List(usage.Filter{ClientName: "alice"}); err != nil || len(events) != 1 {
		t.Fatalf("stored events=%d err=%v, want 1", len(events), err)
	}
}
