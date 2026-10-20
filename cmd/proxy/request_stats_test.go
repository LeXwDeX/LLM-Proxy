package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	bolt "go.etcd.io/bbolt"
)

func TestRequestStatsRetentionIndependentOfUsage(t *testing.T) {
	t.Setenv("USAGE_RETENTION_DAYS", "0")
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := nosql.NewRequestStatsStore(db)
	for _, at := range []time.Time{time.Now().Add(-8 * 24 * time.Hour), time.Now().Add(-time.Hour)} {
		if err := store.Record(requeststats.Event{Timestamp: at, Outcome: requeststats.Success}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startRequestStatsRetention(ctx, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deadline := time.Now().Add(time.Second)
	for {
		count := 0
		if err := db.View(func(tx *bolt.Tx) error { count = tx.Bucket([]byte(nosql.BucketRequestStats)).Stats().KeyN; return nil }); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup retention not run: cells=%d", count)
		}
		time.Sleep(time.Millisecond)
	}
	stats, err := store.Snapshot(time.Now())
	if err != nil || stats.Total != 1 || stats.Since.IsZero() {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}
