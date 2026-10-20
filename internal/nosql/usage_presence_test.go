package nosql

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"testing"
	"time"
)

func TestUsagePresenceDistinguishesObservedZeroFromMissing(t *testing.T) {
	store := NewUsageStore(testDB(t))
	now := time.Now().UTC()
	for _, event := range []usage.Event{
		{Timestamp: now, RequestID: "observed", ObservedFields: usage.UsageFieldInput | usage.UsageFieldOutput | usage.UsageFieldCached, InputTokens: 0, OutputTokens: 0, CachedTokens: 0},
		{Timestamp: now.Add(time.Second), RequestID: "missing"},
	} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.List(usage.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d", len(events))
	}
	for _, event := range events {
		if event.InputTokens != 0 || event.CachedTokens != 0 {
			t.Fatalf("zero counters changed: %+v", event)
		}
		switch event.RequestID {
		case "observed":
			if event.ObservedFields != usage.UsageFieldInput|usage.UsageFieldOutput|usage.UsageFieldCached {
				t.Fatalf("observed zero lost presence: %+v", event)
			}
		case "missing":
			if event.ObservedFields != 0 {
				t.Fatalf("missing fields inferred: %+v", event)
			}
		default:
			t.Fatalf("unexpected event: %+v", event)
		}
	}
}
