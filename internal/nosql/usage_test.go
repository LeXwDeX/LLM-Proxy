package nosql

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestUsageStoreRecordAndList(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	now := time.Now().UTC()
	events := []usage.Event{
		{Timestamp: now.Add(-2 * time.Hour), ClientName: "alice", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50},
		{Timestamp: now.Add(-1 * time.Hour), ClientName: "bob", Model: "gpt-4o", InputTokens: 200, OutputTokens: 100},
		{Timestamp: now, ClientName: "alice", Model: "claude-3", InputTokens: 300, OutputTokens: 150},
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// List all (no filter).
	items, err := store.List(usage.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 events, got %d", len(items))
	}
	// Should be in descending timestamp order.
	if items[0].Timestamp.Before(items[1].Timestamp) {
		t.Fatalf("expected descending order")
	}
}

func TestUsageStoreListFilter(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: base.Add(-3 * time.Hour), ClientName: "alice", Model: "gpt-4o", InputTokens: 100},
		{Timestamp: base.Add(-2 * time.Hour), ClientName: "bob", Model: "gpt-4o", InputTokens: 200},
		{Timestamp: base.Add(-1 * time.Hour), ClientName: "alice", Model: "claude-3", InputTokens: 300},
		{Timestamp: base, ClientName: "bob", Model: "claude-3", InputTokens: 400},
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Filter by time range.
	from := base.Add(-2*time.Hour - 30*time.Minute)
	to := base.Add(-30 * time.Minute)
	items, err := store.List(usage.Filter{From: &from, To: &to})
	if err != nil {
		t.Fatalf("list time range: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 events in time range, got %d", len(items))
	}

	// Filter by client.
	items, err = store.List(usage.Filter{ClientName: "alice"})
	if err != nil {
		t.Fatalf("list client: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 events for alice, got %d", len(items))
	}

	// Filter by model.
	items, err = store.List(usage.Filter{Model: "claude-3"})
	if err != nil {
		t.Fatalf("list model: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 events for claude-3, got %d", len(items))
	}
}

func TestUsageStoreAggregate(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	base := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: base.Add(-25 * time.Hour), ClientName: "alice", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50},
		{Timestamp: base.Add(-1 * time.Hour), ClientName: "bob", Model: "gpt-4o", InputTokens: 200, OutputTokens: 100},
		{Timestamp: base, ClientName: "alice", Model: "claude-3", InputTokens: 300, OutputTokens: 150},
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	from := base.Add(-48 * time.Hour)
	to := base.Add(time.Hour)
	result, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "day", nil)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if result.Totals.Requests != 3 {
		t.Fatalf("expected 3 total requests, got %d", result.Totals.Requests)
	}
	if result.Totals.InputTokens != 600 {
		t.Fatalf("expected 600 total input tokens, got %d", result.Totals.InputTokens)
	}
	if len(result.Buckets) < 1 {
		t.Fatalf("expected at least 1 bucket, got %d", len(result.Buckets))
	}
	if len(result.ByClient) != 2 {
		t.Fatalf("expected 2 client dimensions, got %d", len(result.ByClient))
	}
	if len(result.ByModel) != 2 {
		t.Fatalf("expected 2 model dimensions, got %d", len(result.ByModel))
	}
}

func TestUsageStoreAggregateByClientCostOrder(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	base := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: base.Add(-1 * time.Hour), ClientName: "alice", Model: "gpt-4o", InputTokens: 100},
		{Timestamp: base, ClientName: "bob", Model: "gpt-4o", InputTokens: 500},
		{Timestamp: base, ClientName: "carol", Model: "gpt-4o", InputTokens: 500},
		{Timestamp: base, ClientName: "alice", Model: "claude-3", InputTokens: 50},
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	costs := usage.CostTable{
		"gpt-4o":   usage.CostRates{InputPer1MTokens: 1},
		"claude-3": usage.CostRates{InputPer1MTokens: 10},
	}
	from := base.Add(-2 * time.Hour)
	to := base.Add(time.Hour)
	result, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "day", costs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	// alice 费用最高(0.0006)排第一；bob/carol 并列(0.0005)按名称升序。
	wantClient := []string{"alice", "bob", "carol"}
	if len(result.ByClient) != len(wantClient) {
		t.Fatalf("expected %d client dimensions, got %d", len(wantClient), len(result.ByClient))
	}
	for i, want := range wantClient {
		if result.ByClient[i].Key != want {
			t.Fatalf("by_client order: index %d got %s want %s", i, result.ByClient[i].Key, want)
		}
	}

	// by_model 保持名称升序：gpt-4o 费用更高但仍按字母序排在 claude-3 之后。
	wantModel := []string{"claude-3", "gpt-4o"}
	if len(result.ByModel) != len(wantModel) {
		t.Fatalf("expected %d model dimensions, got %d", len(wantModel), len(result.ByModel))
	}
	for i, want := range wantModel {
		if result.ByModel[i].Key != want {
			t.Fatalf("by_model order: index %d got %s want %s", i, result.ByModel[i].Key, want)
		}
	}
}

func TestUsageStoreCount(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	base := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: base.Add(-3 * time.Hour), ClientName: "alice", StatusCode: 200},
		{Timestamp: base.Add(-2 * time.Hour), ClientName: "bob", StatusCode: 500},
		{Timestamp: base.Add(-1 * time.Hour), ClientName: "alice", StatusCode: 200},
		{Timestamp: base, ClientName: "bob", StatusCode: 429},
		{Timestamp: base.Add(-5 * time.Hour), ClientName: "alice", StatusCode: 200}, // outside window
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Count within a 4-hour window: should see 4 events (base-3h, base-2h, base-1h, base).
	from := base.Add(-4 * time.Hour)
	to := base.Add(time.Second)
	total, success, err := store.Count(from, to)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected 4 total, got %d", total)
	}
	// success: StatusCode 200 (2x) + 429 (1x, <500) = 3; StatusCode 500 is NOT success
	if success != 3 {
		t.Fatalf("expected 3 success, got %d", success)
	}

	// Count full range: should see all 5 events.
	allFrom := base.Add(-6 * time.Hour)
	allTo := base.Add(time.Second)
	total, success, err = store.Count(allFrom, allTo)
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	if total != 5 {
		t.Fatalf("expected 5 total, got %d", total)
	}
	if success != 4 {
		t.Fatalf("expected 4 success, got %d", success)
	}
}

func TestUsageStoreSummary(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	now := time.Date(2026, 4, 5, 15, 0, 0, 0, time.UTC)
	todayStart := time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)

	events := []usage.Event{
		{Timestamp: now.Add(-30 * time.Minute), ClientName: "alice", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50},     // last hour + today
		{Timestamp: todayStart.Add(2 * time.Hour), ClientName: "bob", Model: "gpt-4o", InputTokens: 200, OutputTokens: 100},   // today (not last hour)
		{Timestamp: yesterday, ClientName: "alice", Model: "claude-3", InputTokens: 300, OutputTokens: 150},                   // yesterday
		{Timestamp: now.Add(-10 * 24 * time.Hour), ClientName: "bob", Model: "gpt-4o", InputTokens: 400, OutputTokens: 200},   // last 30 days
		{Timestamp: now.Add(-40 * 24 * time.Hour), ClientName: "alice", Model: "gpt-4o", InputTokens: 500, OutputTokens: 250}, // outside 30 days
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	result, err := store.Summary(now, nil)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	if result.LastHour.Requests != 1 {
		t.Fatalf("expected 1 last hour request, got %d", result.LastHour.Requests)
	}
	if result.Today.Requests != 2 {
		t.Fatalf("expected 2 today requests, got %d", result.Today.Requests)
	}
	if result.Yesterday.Requests != 1 {
		t.Fatalf("expected 1 yesterday request, got %d", result.Yesterday.Requests)
	}
	if result.Last7Days.Requests != 3 {
		t.Fatalf("expected 3 last 7 days requests, got %d", result.Last7Days.Requests)
	}
	if result.Last30Days.Requests != 4 {
		t.Fatalf("expected 4 last 30 days requests, got %d", result.Last30Days.Requests)
	}
}
