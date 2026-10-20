package quota

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// A stalled writer (bbolt hung) must not hold the client's accounting gate
// for long: calibration gives up within the drain bound through the existing
// calibrationFailed path (counters retained, admission blocked), RecordUsage
// is not blocked behind it, and once the writer recovers the next
// calibration counts every event exactly once.
func TestCalibrationWithStalledWriterIsBoundedAndKeepsCharge(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	putQuotaClient(t, db, []config.QuotaGroup{{Name: "domestic"}}, "employee")
	putModelGroup(t, db, config.ModelGroup{Name: "domestic", CostCoefficient: 1, DailyUSD: 5,
		Bindings: []config.QuotaGroupBinding{{Target: "upstream", Model: "qwen-max"}}})
	quotaTestCost(t, db, "qwen-max")
	m.Evaluate("alice")

	primary := &gatedUsageStore{store: m.usageStore, gate: make(chan struct{})}
	journal, err := nosql.NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{DrainWaitLimit: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	m.usageJournal = journal
	charge := func() error { // $1 each
		return m.RecordUsage(journal, usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "upstream",
			EndpointType: "openai", Model: "qwen-max", InputTokens: 100_000, StatusCode: 200})
	}
	used := func() float64 { return m.Status("alice").Groups["domestic"].Quotas[DimensionDaily].Used }
	if err := charge(); err != nil {
		t.Fatal(err)
	}

	evaluated := make(chan struct{})
	go func() {
		defer close(evaluated)
		m.Evaluate("alice")
	}()
	time.Sleep(20 * time.Millisecond) // calibration now waits on the stalled writer
	recorded := make(chan error, 1)
	go func() { recorded <- charge() }()
	select {
	case err := <-recorded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RecordUsage stayed blocked behind the stalled calibration")
	}
	select {
	case <-evaluated:
	case <-time.After(2 * time.Second):
		t.Fatal("calibration did not give up within the drain bound")
	}
	status := m.Status("alice")
	if status.CalibrationError == "" {
		t.Fatalf("abandoned calibration must be reported: %+v", status)
	}
	if got := used(); math.Abs(got-2) > 1e-9 {
		t.Fatalf("counters must be retained and charged once each: used=%v, want 2", got)
	}

	close(primary.gate)
	journal.Flush()
	m.Evaluate("alice")
	status = m.Status("alice")
	if status.CalibrationError != "" || math.Abs(used()-2) > 1e-9 {
		t.Fatalf("recovered calibration: used=%v err=%q, want 2", used(), status.CalibrationError)
	}
	events, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
	if err != nil || len(events) != 2 {
		t.Fatalf("stored events=%d err=%v, want 2", len(events), err)
	}
}
