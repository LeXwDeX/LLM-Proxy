package nosql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Issue #97: admin read handlers pass the request context so a client abort
// stops the scan instead of running to completion on the writer-free reader.
func TestUsageReadersStopOnCanceledContext(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	targets := NewTargetStore(db)
	groups := NewModelGroupStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now
	filter := usage.Filter{From: &from, To: &to}

	if _, err := store.AggregateByModelGroupsAllRequestsContext(ctx, filter, "hour", nil, targets, groups); !errors.Is(err, context.Canceled) {
		t.Errorf("AggregateByModelGroupsAllRequestsContext err = %v, want context.Canceled", err)
	}
	if _, err := store.AggregateByModelGroupsContext(ctx, filter, "hour", nil, targets, groups); !errors.Is(err, context.Canceled) {
		t.Errorf("AggregateByModelGroupsContext err = %v, want context.Canceled", err)
	}
	if _, err := store.AggregateModelGroupsContext(ctx, filter, nil, targets, groups); !errors.Is(err, context.Canceled) {
		t.Errorf("AggregateModelGroupsContext err = %v, want context.Canceled", err)
	}
	if _, err := store.SummaryContext(ctx, now, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("SummaryContext err = %v, want context.Canceled", err)
	}
	// The context-free forms keep working as before.
	if _, err := store.AggregateByModelGroupsAllRequests(filter, "hour", nil, targets, groups); err != nil {
		t.Errorf("context-free aggregate: %v", err)
	}
}
