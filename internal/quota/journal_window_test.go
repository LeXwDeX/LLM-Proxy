package quota

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

func (r accountingRetryRecorder) CheckUnresolvedClientRange(client string, from, to time.Time) error {
	return r.store.CheckUnresolvedClientRange(client, from, to)
}

func (g *gatedUsageStore) CheckUnresolvedClientRange(client string, from, to time.Time) error {
	return g.store.CheckUnresolvedClientRange(client, from, to)
}

func (s *backlogStore) CheckUnresolvedClientRange(client string, from, to time.Time) error {
	return s.store.CheckUnresolvedClientRange(client, from, to)
}

func (r *bundleFaultRecorder) CheckUnresolvedClientRange(client string, from, to time.Time) error {
	return r.store.CheckUnresolvedClientRange(client, from, to)
}

func TestCalibrationJournalUnresolvedWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      func(time.Time) time.Time
		blocked bool
	}{
		{"outside", func(now time.Time) time.Time { return now.AddDate(0, -2, -7) }, false},
		{"inside", func(now time.Time) time.Time { return now.Add(-time.Minute) }, true},
		{"unknown", func(time.Time) time.Time { return time.Time{} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testOpenDB(t)
			m := newTestManager(t, db)
			putQuotaClient(t, db, []config.QuotaGroup{{Name: "models"}}, "employee")
			event := quotaBundle()
			event.Components = nil
			event.Timestamp = tc.at(time.Now().UTC())
			if err := m.usageStore.RegisterUnresolvedUsage(event, []byte(tc.name)); err != nil {
				t.Fatal(err)
			}
			journal, err := nosql.NewUsageJournal(m.usageStore, filepath.Join(t.TempDir(), "usage.jsonl"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			m.usageJournal = journal
			m.Evaluate("alice")
			status := m.Status("alice")
			if (status.CalibrationError != "") != tc.blocked {
				t.Fatalf("calibration error=%q; want blocked=%v", status.CalibrationError, tc.blocked)
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked != tc.blocked || (blocked && info.Scope != ScopeAccounting) {
				t.Fatalf("admission blocked=%v info=%+v; want accounting blocked=%v", blocked, info, tc.blocked)
			}
		})
	}
}
