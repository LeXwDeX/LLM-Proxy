package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hungUsageStore never completes a commit, like a database that stopped
// responding during shutdown.
type hungUsageStore struct{ hang chan struct{} }

func (s hungUsageStore) Record(usage.Event) error        { <-s.hang; return fmt.Errorf("hung") }
func (s hungUsageStore) RecordBatch([]usage.Event) error { <-s.hang; return fmt.Errorf("hung") }

// A drain that cannot finish must not hold the shutdown forever: it gives up
// after the bound with an Error log, and every acknowledged event is still in
// the fsynced journal, so the next start's Replay commits each exactly once.
func TestShutdownDrainTimeoutLeavesUsageForReplay(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "proxy.db")
	journalPath := dbPath + ".usage-journal.jsonl"
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	journal, err := nosql.NewUsageJournal(hungUsageStore{hang: make(chan struct{})}, journalPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	persistence, err := startUsagePersistence(journal, nosql.NewRequestStatsStore(db), logger)
	if err != nil {
		t.Fatal(err)
	}
	if persistence.drainTimeout != persistenceDrainTimeout || persistenceDrainTimeout != 30*time.Second {
		t.Fatalf("default drain timeout = %v", persistence.drainTimeout)
	}
	persistence.drainTimeout = 100 * time.Millisecond
	const events = 20
	now := time.Now().UTC()
	for i := 0; i < events; i++ {
		if err := journal.Record(usage.Event{Timestamp: now.Add(time.Duration(i)), ClientName: "tester", EndpointType: "openai",
			Model: "qwen-max", InputTokens: int64(i + 1), StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	persistence.drain(nil)
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("drain returned after %v, want the 100ms bound", elapsed)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "drain timed out") {
		t.Fatalf("timeout was not logged as an error: %s", logs.String())
	}

	// Next start: the stuck process is gone; replay the same journal.
	store := nosql.NewUsageStore(db)
	restarted, err := nosql.NewUsageJournal(store, journalPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Replay(); err != nil {
		t.Fatalf("startup replay: %v", err)
	}
	if _, err := restarted.Replay(); err != nil {
		t.Fatalf("second replay: %v", err)
	}
	stored, err := store.List(usage.Filter{ClientName: "tester", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != events {
		t.Fatalf("replayed events = %d, want %d", len(stored), events)
	}
	total, _, err := store.Count(now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || total != events {
		t.Fatalf("aggregate total=%d err=%v, want %d (exactly once)", total, err, events)
	}
}
