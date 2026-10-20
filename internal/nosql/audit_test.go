package nosql

import (
	"testing"
	"time"
)

func TestAuditStoreRecordAndList(t *testing.T) {
	db := testDB(t)
	store := NewAuditStore(db)

	// Record multiple events.
	events := []AuditEvent{
		{Actor: "admin", Action: "create_client", Object: "team-a", Result: "ok"},
		{Actor: "admin", Action: "delete_client", Object: "team-b", Result: "ok"},
		{Actor: "admin", Action: "update_cost", Object: "gpt-4o", Result: "ok"},
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
		// Small sleep to ensure different timestamps.
		time.Sleep(time.Millisecond)
	}

	// List should return in reverse chronological order.
	items, err := store.List(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 events, got %d", len(items))
	}
	// First item should be the latest (update_cost).
	if items[0].Action != "update_cost" {
		t.Fatalf("expected latest event action=update_cost, got %q", items[0].Action)
	}
}

func TestAuditStoreAutoFillFields(t *testing.T) {
	db := testDB(t)
	store := NewAuditStore(db)

	// Record with empty ID and zero Timestamp.
	if err := store.Record(AuditEvent{Actor: "admin", Action: "test", Result: "ok"}); err != nil {
		t.Fatalf("record: %v", err)
	}

	items, err := store.List(1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 event, got %d", len(items))
	}
	if items[0].ID == "" {
		t.Fatalf("expected auto-filled ID")
	}
	if items[0].Timestamp.IsZero() {
		t.Fatalf("expected auto-filled Timestamp")
	}
}

func TestAuditStoreListLimit(t *testing.T) {
	db := testDB(t)
	store := NewAuditStore(db)

	// Record 10 events.
	for i := 0; i < 10; i++ {
		if err := store.Record(AuditEvent{Actor: "admin", Action: "test", Result: "ok"}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}

	// List with limit=3.
	items, err := store.List(3)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 events, got %d", len(items))
	}

	// Default limit (0 → 50): should return all 10.
	items, err = store.List(0)
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if len(items) != 10 {
		t.Fatalf("expected 10 events with default limit, got %d", len(items))
	}
}
