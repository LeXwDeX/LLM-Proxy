package admin

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type blockingBatchStore struct {
	store *nosql.UsageStore
	gate  chan struct{}
}

func (s *blockingBatchStore) Record(event usage.Event) error { return s.store.Record(event) }
func (s *blockingBatchStore) RecordBatch(events []usage.Event) error {
	<-s.gate
	return s.store.RecordBatch(events)
}

// /admin/metrics exposes the asynchronous persistence backlog: usage events
// acknowledged but not yet committed, events deferred to replay by a full
// queue, and completion statistics dropped by a full queue.
func TestMetricsExposePersistenceBacklog(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	db := stores.clientStore.DB()
	primary := &blockingBatchStore{store: stores.usageStore, gate: make(chan struct{})}
	journal, err := nosql.NewUsageJournal(primary, filepath.Join(t.TempDir(), "usage.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{QueueSize: 2, MaxBatch: 1}); err != nil {
		t.Fatal(err)
	}
	defer func() { close(primary.gate); journal.Close() }()
	stats := nosql.NewAsyncRequestStatsRecorder(nosql.NewRequestStatsStore(db), nosql.AsyncRequestStatsOptions{QueueSize: 1, MaxBatch: 1}, nil)
	defer stats.Close()

	service, err := proxy.NewService(&config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetUsageRecorder(journal)
	service.SetRequestStatsRecorder(stats)

	for i := 0; i < 6; i++ {
		if err := journal.Record(usage.Event{ClientName: "tester", Model: "qwen-max", InputTokens: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// Stall the statistics writer so its one-slot queue overflows.
	tx, err := db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	dropped := 0
	for i := 0; i < 10; i++ {
		if err := stats.Record(requeststats.Event{Timestamp: time.Now(), Outcome: requeststats.Success}); errors.Is(err, nosql.ErrRequestStatsQueueFull) {
			dropped++
		}
	}
	_ = tx.Rollback()

	h := NewHandler(nil, nil, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, nil)
	w := httptest.NewRecorder()
	h.handleMetrics(w, httptest.NewRequest("GET", "/admin/metrics", nil))
	var body struct {
		Persistence map[string]int64 `json:"persistence"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v %s", err, w.Body.String())
	}
	p := body.Persistence
	if p == nil {
		t.Fatalf("persistence metrics missing: %s", w.Body.String())
	}
	if p["usage_async_pending"] <= 0 || p["usage_async_deferred"] <= 0 {
		t.Fatalf("usage backlog not exposed: %v", p)
	}
	if p["usage_async_pending"]+p["usage_async_deferred"] != 6 {
		t.Fatalf("pending+deferred = %v, want the 6 acknowledged events", p)
	}
	if dropped == 0 || p["request_stats_dropped"] != int64(dropped) {
		t.Fatalf("request_stats_dropped=%d, want %d", p["request_stats_dropped"], dropped)
	}
}
