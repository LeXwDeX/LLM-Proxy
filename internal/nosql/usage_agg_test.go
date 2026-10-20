package nosql

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// TestUsageAggParity is the gold standard: write N random events, then verify
// agg-backed Summary/Aggregate/Count match a brute-force ground truth computed
// from the raw events. Catches regressions in agg key encoding, accumulation,
// time-window boundaries, etc.
func TestUsageAggParity(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	// Fixed seed for reproducibility.
	r := rand.New(rand.NewSource(42))
	now := time.Date(2026, 4, 5, 15, 30, 0, 0, time.UTC) // mid-hour to expose boundary issues
	clients := []string{"alice", "bob", "carol"}
	models := []string{"gpt-4o", "claude-3", "gemini-pro"}
	endpoints := []string{"openai", "claude", "gemini"}

	const N = 500
	rawEvents := make([]usage.Event, 0, N)
	for i := 0; i < N; i++ {
		// Spread events across the past 35 days at random offsets.
		offset := time.Duration(r.Intn(35*24*60)) * time.Minute
		ts := now.Add(-offset)
		evt := usage.Event{
			Timestamp:    ts,
			ClientName:   clients[r.Intn(len(clients))],
			Model:        models[r.Intn(len(models))],
			EndpointType: endpoints[r.Intn(len(endpoints))],
			InputTokens:  int64(r.Intn(1000)),
			OutputTokens: int64(r.Intn(1000)),
			CachedTokens: int64(r.Intn(100)),
			StatusCode:   []int{200, 200, 200, 429, 500}[r.Intn(5)],
		}
		if err := store.Record(evt); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		rawEvents = append(rawEvents, evt)
	}

	// --- Ground truth: brute-force aggregate from rawEvents ---
	gt := bruteForceSummary(rawEvents, now)

	got, err := store.Summary(now, nil)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	checkTotals(t, "last_hour", got.LastHour, gt.LastHour)
	checkTotals(t, "today", got.Today, gt.Today)
	checkTotals(t, "yesterday", got.Yesterday, gt.Yesterday)
	checkTotals(t, "last_7_days", got.Last7Days, gt.Last7Days)
	checkTotals(t, "last_30_days", got.Last30Days, gt.Last30Days)

	// --- Count parity (last 72h) ---
	from72 := now.Add(-72 * time.Hour)
	gotTotal, gotSuccess, err := store.Count(from72, now)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	var wantTotal, wantSuccess int64
	for _, e := range rawEvents {
		t := e.Timestamp.UTC()
		if !t.Before(from72) && !t.After(now) {
			wantTotal++
			if isSuccessStatus(e.StatusCode) {
				wantSuccess++
			}
		}
	}
	// Note: Count uses agg cells (hour granularity). Compare with ground truth
	// computed at the SAME hour granularity to make the comparison fair.
	wantTotal, wantSuccess = bruteForceCountByHour(rawEvents, from72, now)
	if gotTotal != wantTotal {
		t.Errorf("count total: got %d want %d", gotTotal, wantTotal)
	}
	if gotSuccess != wantSuccess {
		t.Errorf("count success: got %d want %d", gotSuccess, wantSuccess)
	}

	// --- Aggregate parity (last 7 days, by day) ---
	from7 := now.AddDate(0, 0, -7)
	to := now
	agg, err := store.Aggregate(usage.Filter{From: &from7, To: &to}, "day", nil)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var wantTotals usage.Totals
	for _, e := range rawEvents {
		t := e.Timestamp.UTC().Truncate(time.Hour)
		if !t.Before(from7.Truncate(time.Hour)) && !t.After(to) {
			wantTotals.Requests++
			wantTotals.InputTokens += e.InputTokens
			wantTotals.OutputTokens += e.OutputTokens
			wantTotals.CachedTokens += e.CachedTokens
		}
	}
	if agg.Totals.Requests != wantTotals.Requests {
		t.Errorf("aggregate requests: got %d want %d", agg.Totals.Requests, wantTotals.Requests)
	}
	if agg.Totals.InputTokens != wantTotals.InputTokens {
		t.Errorf("aggregate input_tokens: got %d want %d", agg.Totals.InputTokens, wantTotals.InputTokens)
	}
	if agg.Totals.OutputTokens != wantTotals.OutputTokens {
		t.Errorf("aggregate output_tokens: got %d want %d", agg.Totals.OutputTokens, wantTotals.OutputTokens)
	}
}

// TestUsageAggBackfillIdempotent verifies that BackfillUsageAgg is no-op on
// re-invocation (meta marker prevents re-scan).
//
// Real-world sequence:
//
//	t0: legacy events present in usage_events, agg bucket empty (fresh upgrade)
//	t1: BackfillUsageAgg #1 -> rebuilds agg, writes meta
//	t2: server starts, Record() double-writes
//	t3: server restarts -> BackfillUsageAgg #2 -> no-op (meta exists)
//
// Test simulates this by writing events directly (bypassing Record's double-write).
func TestUsageAggBackfillIdempotent(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 0, 50)
	for i := 0; i < 50; i++ {
		events = append(events, usage.Event{
			Timestamp:    now.Add(-time.Duration(i) * time.Hour),
			ClientName:   fmt.Sprintf("c%d", i%3),
			Model:        "gpt-4o",
			EndpointType: "openai",
			InputTokens:  100,
			OutputTokens: 50,
			StatusCode:   200,
		})
	}
	// Bypass Record() — write straight into events bucket to simulate legacy data.
	writeRawEvents(t, db, events)

	// First backfill: builds agg from events.
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatalf("backfill 1: %v", err)
	}
	// Summary's non-last-hour windows now read the target-aware quota bucket, so
	// backfill that bucket too (the target-aware equivalent of BackfillUsageAgg).
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota backfill 1: %v", err)
	}
	after1, _ := store.Summary(now, nil)

	// Second backfill: must be a no-op (meta marker exists).
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatalf("backfill 2: %v", err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota backfill 2: %v", err)
	}
	after2, _ := store.Summary(now, nil)

	if after1.Last30Days.Requests != after2.Last30Days.Requests {
		t.Errorf("backfill #2 should be no-op, but requests changed: %d -> %d",
			after1.Last30Days.Requests, after2.Last30Days.Requests)
	}
	if after1.Last30Days.InputTokens != after2.Last30Days.InputTokens {
		t.Errorf("backfill #2 should be no-op, but input_tokens changed: %d -> %d",
			after1.Last30Days.InputTokens, after2.Last30Days.InputTokens)
	}
	if after1.Last30Days.Requests != 50 {
		t.Errorf("expected 50 requests after backfill, got %d", after1.Last30Days.Requests)
	}
}

// TestUsageAggBackfillFromExisting simulates the deploy scenario: usage_events
// bucket pre-populated by the OLD code path (no agg double-write). Backfill
// should rebuild agg correctly from scratch.
func TestUsageAggBackfillFromExisting(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)

	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	rawEvents := make([]usage.Event, 0, 100)
	for i := 0; i < 100; i++ {
		rawEvents = append(rawEvents, usage.Event{
			Timestamp:    now.Add(-time.Duration(i*30) * time.Minute),
			ClientName:   fmt.Sprintf("c%d", i%3),
			Model:        "gpt-4o",
			EndpointType: "openai",
			InputTokens:  100,
			OutputTokens: 50,
			StatusCode:   200,
		})
	}
	// Write straight into events bucket — simulates legacy data from old code path.
	writeRawEvents(t, db, rawEvents)

	if err := BackfillUsageAgg(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Summary's non-last-hour windows now read the target-aware quota bucket.
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota backfill: %v", err)
	}

	gt := bruteForceSummary(rawEvents, now)
	got, _ := store.Summary(now, nil)

	checkTotals(t, "last_30_days(after backfill)", got.Last30Days, gt.Last30Days)
	checkTotals(t, "today(after backfill)", got.Today, gt.Today)
}

func TestQuotaUsageAggBackfillFromEvents(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: now, ClientName: "alice", Target: "Target-A", Model: "Model-A", InputTokens: 100, OutputTokens: 20, StatusCode: 200},
		{Timestamp: now.Add(10 * time.Minute), ClientName: "alice", Target: "target-a", Model: "model-a", InputTokens: 30, OutputTokens: 40, StatusCode: 429},
		{Timestamp: now, ClientName: "alice", Target: "target-b", Model: "model-b", InputTokens: 7, OutputTokens: 8, CachedTokens: 9, CacheCreationTokens: 10, StatusCode: 200},
	}
	writeRawEvents(t, db, events)

	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota backfill: %v", err)
	}
	got, err := store.SumQuotaByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("sum quota usage: %v", err)
	}
	if got["target-a:model-a"] != (UsageTotals{InputTokens: 130, OutputTokens: 60}) {
		t.Errorf("target-a:model-a: got %+v", got["target-a:model-a"])
	}
	if got["target-b:model-b"] != (UsageTotals{InputTokens: 7, OutputTokens: 8, CachedTokens: 9, CacheCreationTokens: 10}) {
		t.Errorf("target-b:model-b: got %+v", got["target-b:model-b"])
	}

	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota backfill repeat: %v", err)
	}
	again, err := store.SumQuotaByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("sum quota usage repeat: %v", err)
	}
	if got["target-a:model-a"] != again["target-a:model-a"] || got["target-b:model-b"] != again["target-b:model-b"] {
		t.Errorf("repeated backfill changed quota aggregate: %v -> %v", got, again)
	}
}

func TestQuotaUsageAggBackfillSkipsInvalidEvents(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if err := b.Put([]byte("2026-04-05T11:00:00Z_invalid"), []byte("not-json")); err != nil {
			return err
		}
		data, err := json.Marshal(usage.Event{
			Timestamp: now, ClientName: "alice", Target: "target", Model: "model", InputTokens: 11,
		})
		if err != nil {
			return err
		}
		return b.Put([]byte("2026-04-05T12:00:00Z_valid"), data)
	})
	if err != nil {
		t.Fatalf("write events: %v", err)
	}

	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota backfill: %v", err)
	}
	got, err := store.SumQuotaByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("sum quota usage: %v", err)
	}
	if got["target:model"].InputTokens != 11 {
		t.Errorf("invalid event blocked valid event: got %+v", got)
	}
}

func TestQuotaAggKeyEncodingRoundTrip(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Date(2026, 4, 5, 12, 30, 0, 0, time.UTC)
	evt := usage.Event{
		Timestamp:   now,
		ClientName:  "alice",
		Target:      "pipe|target",
		Model:       "ft:model:v1",
		InputTokens: 42,
		StatusCode:  200,
	}
	if err := store.Record(evt); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := store.SumQuotaByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("sum quota: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one group key, got %d: %v", len(got), got)
	}
	for key := range got {
		target, model := SplitQuotaGroupKey(key)
		if target != "pipe|target" || model != "ft:model:v1" {
			t.Errorf("SplitQuotaGroupKey(%q) = (%q, %q), want (pipe|target, ft:model:v1)", key, target, model)
		}
	}
}

func TestQuotaSumLegacyFallbackAccumulates(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	hour := now.UTC().Truncate(time.Hour).Format(hourLayout)

	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageAggHourly))
		if b == nil {
			t.Fatal("usage_agg_hourly bucket missing")
		}
		cells := []struct {
			key  string
			cell AggCell
		}{
			{hour + "|openai|alice|gpt-4o", AggCell{InputTokens: 100}},
			{hour + "|claude|alice|gpt-4o", AggCell{InputTokens: 50}},
		}
		for _, c := range cells {
			data, err := json.Marshal(c.cell)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(c.key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("write legacy cells: %v", err)
	}

	got, err := store.SumQuotaByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("sum quota: %v", err)
	}
	if got[":gpt-4o"].InputTokens != 150 {
		t.Errorf("legacy fallback did not accumulate: got %+v", got)
	}
}

func TestQuotaUsageAggBackfillBatchesEvents(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	const eventCount = usageBackfillBatchSize + 1
	events := make([]usage.Event, 0, eventCount)
	for i := 0; i < eventCount; i++ {
		events = append(events, usage.Event{
			Timestamp: now, ClientName: "alice", Target: "target", Model: "model", InputTokens: 1,
		})
	}
	writeRawEvents(t, db, events)

	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("quota batch backfill: %v", err)
	}
	got, err := store.SumQuotaByClientRange("alice", now, now)
	if err != nil {
		t.Fatalf("sum quota usage: %v", err)
	}
	if got["target:model"].InputTokens != eventCount {
		t.Errorf("batched backfill input_tokens: got %d want %d", got["target:model"].InputTokens, eventCount)
	}
}

// writeRawEvents writes events directly to the events bucket, bypassing Record's
// double-write. Used to simulate "legacy data from old code path".
func writeRawEvents(t *testing.T, db *bolt.DB, events []usage.Event) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		for i, evt := range events {
			data, err := json.Marshal(evt)
			if err != nil {
				return err
			}
			// Key must sort by timestamp; append index for uniqueness.
			key := fmt.Sprintf("%s_%05d", evt.Timestamp.Format(time.RFC3339Nano), i)
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("writeRawEvents: %v", err)
	}
}

// --- helpers ---

func bruteForceSummary(events []usage.Event, now time.Time) usage.SummaryResult {
	now = now.UTC()
	r := usage.SummaryResult{GeneratedAt: now}
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lastHourStart := now.Add(-1 * time.Hour)
	yesterdayDay := now.AddDate(0, 0, -1)
	yStart := time.Date(yesterdayDay.Year(), yesterdayDay.Month(), yesterdayDay.Day(), 0, 0, 0, 0, time.UTC)
	yEnd := yStart.Add(24 * time.Hour)
	last7Start := now.AddDate(0, 0, -7)
	last30Start := now.AddDate(0, 0, -30)

	add := func(t *usage.Totals, e usage.Event) {
		t.Requests++
		t.InputTokens += e.InputTokens
		t.OutputTokens += e.OutputTokens
		t.CachedTokens += e.CachedTokens
	}

	for _, e := range events {
		t := e.Timestamp.UTC()
		if t.After(now) {
			continue
		}
		// last_hour: precise (raw event time)
		if !t.Before(lastHourStart) {
			add(&r.LastHour, e)
		}
		// other windows: align to hour bucket to match the agg implementation
		hr := t.Truncate(time.Hour)
		if !hr.Before(last30Start) {
			add(&r.Last30Days, e)
			if !hr.Before(last7Start) {
				add(&r.Last7Days, e)
			}
			if !hr.Before(todayStart) {
				add(&r.Today, e)
			}
			if !hr.Before(yStart) && hr.Before(yEnd) {
				add(&r.Yesterday, e)
			}
		}
	}
	return r
}

func bruteForceCountByHour(events []usage.Event, from, to time.Time) (total, success int64) {
	fromHr := from.UTC().Truncate(time.Hour)
	toHr := to.UTC().Truncate(time.Hour)
	for _, e := range events {
		hr := e.Timestamp.UTC().Truncate(time.Hour)
		if !hr.Before(fromHr) && !hr.After(toHr) {
			total++
			if isSuccessStatus(e.StatusCode) {
				success++
			}
		}
	}
	return
}

func checkTotals(t *testing.T, label string, got, want usage.Totals) {
	t.Helper()
	if got.Requests != want.Requests {
		t.Errorf("%s requests: got %d want %d", label, got.Requests, want.Requests)
	}
	if got.InputTokens != want.InputTokens {
		t.Errorf("%s input_tokens: got %d want %d", label, got.InputTokens, want.InputTokens)
	}
	if got.OutputTokens != want.OutputTokens {
		t.Errorf("%s output_tokens: got %d want %d", label, got.OutputTokens, want.OutputTokens)
	}
	if got.CachedTokens != want.CachedTokens {
		t.Errorf("%s cached_tokens: got %d want %d", label, got.CachedTokens, want.CachedTokens)
	}
}

func wipeAggForTest(t *testing.T, db *bolt.DB) {
	t.Helper()
	_ = db
}

// BenchmarkSummary_AggBacked measures the new agg-backed Summary path.
// Compare against the brute-force baseline below to see speedup.
func BenchmarkSummary_AggBacked(b *testing.B) {
	db := benchSeedDB(b, 50_000)
	store := NewUsageStore(db)
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.Summary(now, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSummary_FullScanBaseline simulates the OLD impl: full 30-day events
// scan + Unmarshal. Run as a baseline to quantify the agg speedup.
func BenchmarkSummary_FullScanBaseline(b *testing.B) {
	db := benchSeedDB(b, 50_000)
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	from := now.AddDate(0, 0, -30)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var totals usage.Totals
		err := db.View(func(tx *bolt.Tx) error {
			bk := tx.Bucket([]byte(BucketUsageEvents))
			c := bk.Cursor()
			for k, v := c.Seek([]byte(from.Format(time.RFC3339Nano))); k != nil; k, v = c.Next() {
				var evt usage.Event
				if err := json.Unmarshal(v, &evt); err != nil {
					continue
				}
				if evt.Timestamp.Before(from) || evt.Timestamp.After(now) {
					continue
				}
				totals.Requests++
				totals.InputTokens += evt.InputTokens
				totals.OutputTokens += evt.OutputTokens
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		_ = totals
	}
}

// benchSeedDB seeds N events spread over the past 30 days, using one bulk
// transaction (per-event Record would be 100x slower and dominate the bench).
func benchSeedDB(b *testing.B, n int) *bolt.DB {
	b.Helper()
	db := testDB(b)
	r := rand.New(rand.NewSource(1))
	now := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	err := db.Update(func(tx *bolt.Tx) error {
		eb := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < n; i++ {
			offset := time.Duration(r.Intn(30*24*60)) * time.Minute
			evt := usage.Event{
				Timestamp:    now.Add(-offset),
				ClientName:   fmt.Sprintf("c%d", r.Intn(20)),
				Model:        fmt.Sprintf("m%d", r.Intn(10)),
				EndpointType: "openai",
				InputTokens:  int64(r.Intn(2000)),
				OutputTokens: int64(r.Intn(2000)),
				StatusCode:   200,
			}
			data, _ := json.Marshal(evt)
			key := fmt.Sprintf("%s_%07d", evt.Timestamp.Format(time.RFC3339Nano), i)
			if err := eb.Put([]byte(key), data); err != nil {
				return err
			}
			if err := bumpAgg(tx, evt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return db
}

func TestQuotaBumpAggCorruptCellReturnsError(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 4, 5, 12, 30, 0, 0, time.UTC)
	evt := usage.Event{Timestamp: now, ClientName: "alice", Target: "target", Model: "model", InputTokens: 1}
	key := quotaAggKey(evt.Timestamp, evt.ClientName, evt.Target, evt.Model)

	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(key, []byte("not-json"))
	}); err != nil {
		t.Fatalf("seed corrupt quota agg cell: %v", err)
	}

	err := db.Update(func(tx *bolt.Tx) error {
		return bumpQuotaAgg(tx, evt)
	})
	if err == nil {
		t.Fatal("expected corrupt quota agg cell to be rejected")
	}
	if !strings.Contains(err.Error(), "decode quota agg cell") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The corrupted cell must remain untouched after the rejected write.
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Get(key)
		if string(raw) != "not-json" {
			t.Fatalf("corrupt quota agg cell was modified: %q", raw)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify raw value: %v", err)
	}
}
