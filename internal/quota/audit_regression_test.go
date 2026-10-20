package quota

import (
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type auditRecorderFunc func(usage.Event) error

func (f auditRecorderFunc) Record(e usage.Event) error { return f(e) }

func TestAuditRegressionCalibrationWaitsForCompleteAccounting(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)
	defer m.Stop()
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelCost(t, db, nosql.ModelCost{EndpointType: "openai", Model: "model", InputPer1MTokens: 10})
	m.Evaluate("alice")
	persisted := make(chan struct{}, 2)
	release := make(chan struct{})
	finished := make(chan error, 2)
	store := nosql.NewUsageStore(db)
	recorder := auditRecorderFunc(func(e usage.Event) error {
		if err := store.Record(e); err != nil {
			return err
		}
		persisted <- struct{}{}
		<-release
		return nil
	})
	for i := 0; i < 2; i++ {
		go func() {
			finished <- m.RecordUsage(recorder, usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", EndpointType: "openai", Model: "model", InputTokens: 500000, StatusCode: 200})
		}()
	}
	// Both writes must enter concurrently, preserving bbolt's batching ability.
	for i := 0; i < 2; i++ {
		select {
		case <-persisted:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("accounting serialized concurrent records")
		}
	}
	calibrated := make(chan struct{})
	go func() { m.Evaluate("alice"); close(calibrated) }()
	select {
	case <-calibrated:
		close(release)
		<-finished
		<-finished
		t.Fatal("calibration entered the persist/increment gap")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	<-calibrated
	if used := m.Status("alice").Groups["company_general"].Quotas[DimensionDaily].Used; used != 10 {
		t.Fatalf("cost counted incorrectly: %v, want 10", used)
	}
	writeErr := errors.New("record unavailable")
	if err := m.RecordUsage(auditRecorderFunc(func(usage.Event) error { return writeErr }), usage.Event{ClientName: "alice", EndpointType: "openai", Model: "model", InputTokens: 500000}); !errors.Is(err, writeErr) {
		t.Fatalf("write error lost: %v", err)
	}
	if used := m.Status("alice").Groups["company_general"].Quotas[DimensionDaily].Used; used != 10 {
		t.Fatalf("failed write created a memory-only charge: %v", used)
	}
}
