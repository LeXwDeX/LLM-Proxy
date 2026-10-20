package nosql

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"testing"
	"time"
)

func TestAuditRegressionUsageFractionalOrdering(t *testing.T) {
	store := NewUsageStore(testDB(t))
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{0, 100 * time.Millisecond, 150 * time.Millisecond} {
		if err := store.Record(usage.Event{Timestamp: base.Add(offset), ClientName: "audit"}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List(usage.Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].Timestamp.Equal(base.Add(150*time.Millisecond)) {
		t.Errorf("latest event incorrect: %+v", items)
	}
	to := base.Add(150 * time.Millisecond)
	items, err = store.List(usage.Filter{To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Errorf("inclusive to dropped in-range events: got %d, want 3", len(items))
	}
}

func TestAuditRegressionOperationFractionalOrdering(t *testing.T) {
	store := NewAuditStore(testDB(t))
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-time.Second, 0, 100 * time.Millisecond, 150 * time.Millisecond, 101 * time.Millisecond} {
		if err := store.Record(AuditEvent{Timestamp: base.Add(offset)}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.List(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !events[0].Timestamp.Equal(base.Add(150*time.Millisecond)) || !events[1].Timestamp.Equal(base.Add(101*time.Millisecond)) {
		t.Fatalf("incorrect latest events: %+v", events)
	}
}
