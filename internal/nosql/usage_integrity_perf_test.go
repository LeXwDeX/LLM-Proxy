package nosql

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Issue #125 phase 6: Fast versus Full timing as history outside the read
// window grows while the window itself stays the same. Opt-in because the
// largest fixture takes minutes to build:
//
//	LLMPROXY_INTEGRITY_PERF=1 go test ./internal/nosql/ -run TestIntegrityPerformanceScaling -v -timeout 60m
func TestIntegrityPerformanceScaling(t *testing.T) {
	if os.Getenv("LLMPROXY_INTEGRITY_PERF") != "1" {
		t.Skip("set LLMPROXY_INTEGRITY_PERF=1 to measure")
	}
	const clients, inWindow = 10, 2000
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, total := range []int{4_000, 30_000, 120_000} {
		db := testDB(t)
		store := NewUsageStore(db)
		store.now = func() time.Time { return now }
		batch := make([]usage.Event, 0, 1000)
		flush := func() {
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
		for i := 0; i < total; i++ {
			var at time.Time
			if i < inWindow {
				// The last 20 days: inside the calibration month and Summary's 30 days.
				at = now.Add(-time.Duration(1+i%(20*24)) * time.Hour).Add(time.Duration(i%3600) * time.Second)
			} else {
				// Older history, 40..400 days back: outside every read window.
				at = now.Add(-time.Duration(40*24+i%(360*24)) * time.Hour).Add(time.Duration(i%3600) * time.Second)
			}
			batch = append(batch, usage.Event{
				RecordID: fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i), Timestamp: at,
				ClientName: fmt.Sprintf("client-%02d", i%clients), Target: "anthropic", Model: "executor",
				EndpointType: "claude", Path: "/v1/messages", StatusCode: 200, InputTokens: 100, OutputTokens: 10,
			})
			if len(batch) == cap(batch) {
				flush()
			}
		}
		flush()
		migrateClientIndex(t, db)
		resolver := exactCostTable{replayCosts}
		from := now.AddDate(0, -1, 0)
		calibrateAll := func() time.Duration {
			started := time.Now()
			for c := 0; c < clients; c++ {
				if _, err := store.ReadQuotaCostUnits(context.Background(), usage.Filter{ClientName: fmt.Sprintf("client-%02d", c), From: &from, To: &now}, resolver, true); err != nil {
					t.Fatal(err)
				}
			}
			return time.Since(started)
		}
		summary := func() time.Duration {
			started := time.Now()
			if _, err := store.Summary(now, resolver); err != nil {
				t.Fatal(err)
			}
			return time.Since(started)
		}
		saved := integrityForceFast.Load()
		integrityForceFast.Store(false)
		ForgetUsageIntegrityClassification(db)
		fullCal, fullSum := calibrateAll(), summary()
		classStarted := time.Now()
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		classified := time.Since(classStarted)
		stats := UsageIntegrityStats(db)
		if !stats.Complete {
			t.Fatalf("classification incomplete: %+v", stats)
		}
		fastCal, fastSum := calibrateAll(), summary()
		if got := UsageIntegrityStats(db).FastReads - stats.FastReads; got < clients {
			t.Fatalf("fast reads=%d, want at least %d", got, clients)
		}
		integrityForceFast.Store(saved)
		ForgetUsageIntegrityClassification(db)
		t.Logf("events=%d in_window=%d clients=%d anomalous=%d | calibrateAll Full=%v Fast=%v | Summary Full=%v Fast=%v | classification=%v",
			total, inWindow, clients, stats.AnomalousCount, fullCal.Round(time.Millisecond), fastCal.Round(time.Millisecond), fullSum.Round(time.Millisecond), fastSum.Round(time.Millisecond), classified.Round(time.Millisecond))
	}
}
