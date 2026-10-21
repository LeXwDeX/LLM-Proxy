package quota

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func writePendingJournalLine(t *testing.T, path string, event usage.Event) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCalibrationRecoversAfterPermanentJournalRejection: a pending line that
// conflicts with a committed identity is moved aside, so admission recovers
// with counters rebuilt from the ledger exactly once.
func TestCalibrationRecoversAfterPermanentJournalRejection(t *testing.T) {
	m := bundleManager(t, 1)
	committed := quotaBundle()
	if err := m.RecordUsage(m.usageStore, committed); err != nil {
		t.Fatal(err)
	}
	conflicting := committed
	conflicting.InputTokens = 9_000_000
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	writePendingJournalLine(t, path, conflicting)
	journal, err := nosql.NewUsageJournal(m.usageStore, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal

	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError != "" {
		t.Fatalf("permanent rejection still fails calibration: %s", status.CalibrationError)
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
		t.Fatalf("admission still blocked after rejection moved aside: %+v", info)
	}
	if got := bundleUsed(m); got != 12 {
		t.Fatalf("calibrated usage=%v, want 12 (committed bundle once, conflicting line not charged)", got)
	}
	if _, err := os.Stat(path + ".rejected"); err != nil {
		t.Fatalf("rejected line was not preserved: %v", err)
	}
}

// TestCalibrationStaysBlockedOnTransientJournalFailure is the fail-closed
// counterexample: a primary write failure keeps the client blocked.
func TestCalibrationStaysBlockedOnTransientJournalFailure(t *testing.T) {
	m := bundleManager(t, 1)
	pending := quotaBundle()
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	writePendingJournalLine(t, path, pending)
	writeErr := errors.New("disk write failed")
	primary := accountingRetryRecorder{store: m.usageStore, auditRecorderFunc: func(usage.Event) error { return writeErr }}
	journal, err := nosql.NewUsageJournal(primary, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.usageJournal = journal

	m.Evaluate("alice")
	if status := m.Status("alice"); status.CalibrationError == "" {
		t.Fatal("transient journal failure did not fail calibration")
	}
	if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
		t.Fatalf("transient journal failure admitted: %+v blocked=%t", info, blocked)
	}
	if _, err := os.Stat(path + ".rejected"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transient failure wrote a rejection sidecar: %v", err)
	}
}
