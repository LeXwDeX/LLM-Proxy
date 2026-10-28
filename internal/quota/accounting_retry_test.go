package quota

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type accountingRetryRecorder struct {
	auditRecorderFunc
	store *nosql.UsageStore
}

func (r accountingRetryRecorder) RecordExists(event usage.Event) (bool, error) {
	return r.store.RecordExists(event)
}

func (r accountingRetryRecorder) RegisterUnresolvedUsage(event usage.Event, original []byte) error {
	return r.store.RegisterUnresolvedUsage(event, original)
}

func expireCalibrationBackoff(m *Manager) {
	m.mu.Lock()
	m.calibrationRetryAt["alice"] = time.Now().Add(-time.Second)
	m.mu.Unlock()
}

func TestRecordUsageCalibrationRetryBackoffAndRecovery(t *testing.T) {
	for _, trigger := range []string{"completion", "admission"} {
		t.Run(trigger, func(t *testing.T) {
			m := bundleManager(t, 1)
			var attempts atomic.Int64
			var healthy atomic.Bool
			draining := make(chan struct{})
			releaseDrain := make(chan struct{})
			var drainOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseDrain) }) }
			t.Cleanup(release)
			primary := accountingRetryRecorder{store: m.usageStore, auditRecorderFunc: func(event usage.Event) error {
				attempts.Add(1)
				if !healthy.Load() {
					return errors.New("primary unavailable")
				}
				drainOnce.Do(func() { close(draining) })
				<-releaseDrain
				return m.usageStore.Record(event)
			}}
			journal, err := nosql.NewUsageJournal(primary, filepath.Join(t.TempDir(), "pending.jsonl"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { release(); _ = journal.Close() })
			m.usageJournal = journal
			pending := quotaBundle()
			if err := m.RecordUsage(journal, pending); err != nil {
				t.Fatal(err)
			}
			attempts.Store(0)
			writeErr := errors.New("uncertain record failure")
			if err := m.RecordUsage(auditRecorderFunc(func(usage.Event) error { return writeErr }), quotaBundle()); !errors.Is(err, writeErr) {
				t.Fatalf("record error=%v, want %v", err, writeErr)
			}
			if got := attempts.Load(); got != 0 {
				t.Fatalf("failed record retried calibration %d times", got)
			}

			// All eight records have persisted before any can increment or reconcile.
			completeBurst := func() {
				t.Helper()
				entered := make(chan struct{}, 8)
				proceed := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(proceed) }) }
				defer unblock()
				done := make(chan error, 8)
				recorder := auditRecorderFunc(func(event usage.Event) error {
					if err := m.usageStore.Record(event); err != nil {
						return err
					}
					entered <- struct{}{}
					<-proceed
					return nil
				})
				for i := 0; i < cap(done); i++ {
					event := quotaBundle()
					event.Components = nil
					go func() { done <- m.RecordUsage(recorder, event) }()
				}
				for i := 0; i < cap(done); i++ {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("records did not persist concurrently")
					}
				}
				unblock()
				for i := 0; i < cap(done); i++ {
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("completion reconciliation deadlocked")
					}
				}
			}
			assertBlocked := func() {
				t.Helper()
				if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); !blocked || info.Scope != ScopeAccounting {
					t.Fatalf("unreconciled accounting admitted: %+v blocked=%t", info, blocked)
				}
			}
			completeBurst()
			assertBlocked()
			if got := attempts.Load(); got != 0 {
				t.Fatalf("completion burst bypassed calibration backoff: attempts=%d, want 0", got)
			}
			expireCalibrationBackoff(m)
			completeBurst()
			assertBlocked()
			if got := attempts.Load(); got != 1 {
				t.Fatalf("due completion burst retried calibration %d times, want 1", got)
			}
			healthy.Store(true)
			completeBurst()
			assertBlocked()
			if got := attempts.Load(); got != 1 || bundleUsed(m) != 60 {
				t.Fatalf("backoff lost charge or retried after repair: attempts=%d used=%v", got, bundleUsed(m))
			}

			expireCalibrationBackoff(m)
			done := make(chan error, 1)
			go func() {
				if trigger == "completion" {
					done <- m.RecordUsage(journal, pending)
					return
				}
				if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
					done <- errors.New("recovered admission still blocked: " + info.Scope)
					return
				}
				done <- nil
			}()
			select {
			case <-draining:
			case <-time.After(5 * time.Second):
				t.Fatal("due retry did not drain pending usage")
			}
			if info, blocked := m.Check("alice"); !blocked || info.Scope != ScopeAccounting || bundleUsed(m) != 60 {
				t.Fatalf("pending drain cleared accounting failure or charge: %+v blocked=%t used=%v", info, blocked, bundleUsed(m))
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovered retry did not finish")
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("recovery drain attempts=%d, want 2 total", got)
			}
			if info, blocked := m.CheckTarget("alice", "anthropic", "executor"); blocked {
				t.Fatalf("authoritative recovery did not restore admission: %+v", info)
			}
			if err := m.RecordUsage(journal, pending); err != nil {
				t.Fatal(err)
			}
			if status := m.Status("alice"); status.CalibrationError != "" || bundleUsed(m) != 60 {
				t.Fatalf("drain/retry lost or doubled charge: %+v", status)
			}
			rows, err := m.usageStore.List(usage.Filter{ClientName: "alice"})
			if err != nil || len(rows) != 27 {
				t.Fatalf("durable rows=%d err=%v, want 24 roots and one 3-row bundle", len(rows), err)
			}
		})
	}
}
