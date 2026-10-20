package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// Shutdown regression: stop accepting, wait for in-flight handlers, drain the
// usage and statistics queues, replay the journal, and only then close the
// database. Every event queued at shutdown must be in bbolt afterwards.
func TestShutdownDrainPersistsEveryQueuedEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "proxy.db")
	journalPath := dbPath + ".usage-journal.jsonl"
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store := nosql.NewUsageStore(db)
	statsStore := nosql.NewRequestStatsStore(db)
	journal, err := nosql.NewUsageJournal(store, journalPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	persistence, err := startUsagePersistence(journal, statsStore, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Stall every bbolt write so all events stay queued until the drain.
	stall, err := db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := func(i int) error {
		if err := journal.Record(usage.Event{Timestamp: now, ClientName: "tester", EndpointType: "openai",
			Model: "qwen-max", InputTokens: int64(i + 1), StatusCode: 200, RequestID: fmt.Sprintf("req-%d", i)}); err != nil {
			return err
		}
		return persistence.stats.Record(requeststats.Event{Timestamp: now, Outcome: requeststats.Success})
	}
	const background = 100
	var wg sync.WaitGroup
	for i := 0; i < background; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := record(i); err != nil {
				t.Errorf("record %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// One request is still in flight when the shutdown signal arrives.
	entered, release := make(chan struct{}), make(chan struct{})
	server := &http.Server{Addr: freeLoopbackAddr(t), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if err := record(background); err != nil {
			t.Errorf("in-flight record: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- serveAndDrain(ctx, server, 10*time.Second, logger, func(context.Context) error { return nil })
	}()
	responded := make(chan int, 1)
	go func() {
		var resp *http.Response
		var err error
		for attempt := 0; attempt < 200; attempt++ {
			resp, err = http.Get("http://" + server.Addr + "/")
			if err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err != nil {
			t.Errorf("in-flight request: %v", err)
			responded <- 0
			return
		}
		resp.Body.Close()
		responded <- resp.StatusCode
	}()
	<-entered
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-served; err != nil {
		t.Fatalf("serveAndDrain: %v", err)
	}
	if code := <-responded; code != http.StatusOK {
		t.Fatalf("in-flight request status %d", code)
	}

	// The database is still stalled: the drain must wait for the commits.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = stall.Rollback()
	}()
	persistence.drain(nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := nosql.NewUsageStore(reopened).List(usage.Filter{ClientName: "tester", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != background+1 {
		t.Fatalf("usage events after shutdown = %d, want %d", len(events), background+1)
	}
	snapshot, err := nosql.NewRequestStatsStore(reopened).Snapshot(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Total != background+1 {
		t.Fatalf("completions after shutdown = %d, want %d", snapshot.Total, background+1)
	}
	if info, err := os.Stat(journalPath); err == nil && info.Size() != 0 {
		t.Fatalf("recovery journal still holds %d bytes after a clean shutdown", info.Size())
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
