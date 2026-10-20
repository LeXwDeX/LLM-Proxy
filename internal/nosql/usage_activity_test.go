package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestRecentActivityExactBoundariesAndTokenPartitions(t *testing.T) {
	for _, from := range []time.Time{
		time.Date(2026, 10, 14, 12, 59, 59, 900000000, time.UTC),
		time.Date(2026, 10, 14, 12, 59, 59, 0, time.UTC),
	} {
		t.Run(from.Format(time.RFC3339Nano), func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			to := from.Add(2 * time.Second)
			var events []usage.Event
			for i, at := range []time.Time{from.Add(-time.Nanosecond), from, from.Add(time.Nanosecond), from.Add(time.Second), to.Add(-time.Nanosecond), to, to.Add(time.Nanosecond)} {
				name := " Alice "
				if i%2 == 0 {
					name = "ALICE"
				}
				events = append(events, usage.Event{Timestamp: at, ClientName: name, InputTokens: 1, CachedTokens: 2, CacheCreationTokens: 3, ToolUsePromptTokens: 4, OutputTokens: 5, ReasoningTokens: 6, ImageInputTokens: 7, CachedImageInputTokens: 8, ImageOutputTokens: 9, ExplicitCacheReadTokens: 10, CacheCreation5mTokens: 11, CacheCreation1hTokens: 12, UsageStatus: usage.UsageStatusIncomplete})
			}
			writeRawEvents(t, db, events)
			got, err := store.RecentActivity(context.Background(), from, to, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if got.Totals.Requests != 4 || got.Totals.InputTokens != 4 || got.Totals.CachedTokens != 8 || got.Totals.CacheCreationTokens != 12 || got.Totals.ToolUsePromptTokens != 16 || got.Totals.OutputTokens != 20 || got.Totals.IncompleteUsageCount != 4 {
				t.Fatalf("[from,to) token totals: %+v", got.Totals)
			}
			if len(got.Clients) != 1 || got.Clients["alice"].Totals.Requests != 4 {
				t.Fatalf("name normalization: %+v", got.Clients)
			}
			var requests int64
			for _, bucket := range got.Series {
				requests += bucket.Requests
			}
			if requests != 4 {
				t.Fatal("series and global totals differ")
			}
		})
	}
}

func TestRecentActivityDoesNotTruncateThousandsOfEvents(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	to := time.Date(2026, 10, 14, 13, 0, 0, 123, time.UTC)
	from := to.Add(-5 * time.Minute)
	events := make([]usage.Event, 5001)
	for i := range events {
		name := fmt.Sprintf("Client-%03d", i%300)
		if i%2 == 0 {
			name = strings.ToLower(name)
		}
		events[i] = usage.Event{Timestamp: from.Add(time.Duration(i+1) * time.Millisecond), ClientName: name, InputTokens: 1, OutputTokens: 2}
	}
	writeRawEvents(t, db, events)
	got, err := store.RecentActivity(context.Background(), from, to, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.Requests != 5001 || got.Totals.InputTokens != 5001 || got.Totals.OutputTokens != 10002 || len(got.Clients) != 300 || len(got.Series) != 31 {
		t.Fatalf("truncated aggregate: totals=%+v clients=%d buckets=%d", got.Totals, len(got.Clients), len(got.Series))
	}
	var count int64
	for _, client := range got.Clients {
		count += client.Totals.Requests
	}
	if count != 5001 {
		t.Fatal("client aggregate lost events")
	}
}

type activityCancelContext struct {
	context.Context
	calls int
}

func (c *activityCancelContext) Err() error {
	c.calls++
	if c.calls >= 5 {
		return context.Canceled
	}
	return nil
}

func TestRecentActivityFailuresDoNotReturnPartialZero(t *testing.T) {
	for _, kind := range []string{"canceled", "mid-scan canceled", "corrupt", "missing bucket", "closed"} {
		t.Run(kind, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			from := time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC)
			to := from.Add(5 * time.Minute)
			events := make([]usage.Event, 500)
			for i := range events {
				events[i] = usage.Event{Timestamp: from.Add(time.Duration(i) * time.Millisecond), ClientName: "known", InputTokens: 1}
			}
			writeRawEvents(t, db, events)
			ctx := context.Context(context.Background())
			switch kind {
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "mid-scan canceled":
				ctx = &activityCancelContext{Context: ctx}
			case "corrupt":
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(from.Add(time.Second), "corrupt")), []byte("not-json-sensitive"))
				}); err != nil {
					t.Fatal(err)
				}
			case "missing bucket":
				if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketUsageEvents)) }); err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.RecentActivity(ctx, from, to, 10*time.Second)
			if err == nil || got.Clients != nil || got.Series != nil {
				t.Fatalf("failed read must not return usable partial counts: %+v err=%v", got, err)
			}
			if strings.Contains(kind, "canceled") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was swallowed: %v", err)
			}
		})
	}
}

func BenchmarkRecentActivity5000Events300Clients(b *testing.B) {
	db := testDB(b)
	store := NewUsageStore(db)
	to := time.Date(2026, 10, 14, 13, 0, 0, 1, time.UTC)
	from := to.Add(-5 * time.Minute)
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketUsageEvents))
		for i := 0; i < 5000; i++ {
			evt := usage.Event{Timestamp: from.Add(time.Duration(i+1) * time.Millisecond), ClientName: fmt.Sprintf("Client-%03d", i%300), InputTokens: 10, OutputTokens: 5}
			data, err := json.Marshal(evt)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(usageKey(evt.Timestamp, fmt.Sprint(i))), data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.RecentActivity(context.Background(), from, to, 10*time.Second); err != nil {
			b.Fatal(err)
		}
	}
}
