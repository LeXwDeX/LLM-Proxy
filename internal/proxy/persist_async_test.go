package proxy

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

const persistSSEBody = "data: {\"id\":\"c1\",\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"model\":\"test-model\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
	"data: [DONE]\n\n"

type persistenceFixture struct {
	db      *bolt.DB
	store   *nosql.UsageStore
	stats   *nosql.RequestStatsStore
	journal *nosql.UsageJournal
	async   *nosql.AsyncRequestStatsRecorder
	front   *httptest.Server
	service *Service
}

// newPersistenceFixture wires the production persistence path (async
// write-ahead usage journal + async request stats) unless legacy is set, in
// which case it reproduces the previous synchronous wiring with bbolt's
// default 10ms batch delay for before/after comparisons.
func newPersistenceFixture(tb testing.TB, legacy bool) *persistenceFixture {
	tb.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, event := range strings.SplitAfter(persistSSEBody, "\n\n") {
			if event == "" {
				continue
			}
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	tb.Cleanup(upstream.Close)
	dir := tb.TempDir()
	db, err := nosql.OpenDB(filepath.Join(dir, "proxy.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	f := &persistenceFixture{db: db, store: nosql.NewUsageStore(db), stats: nosql.NewRequestStatsStore(db)}
	f.journal, err = nosql.NewUsageJournal(f.store, filepath.Join(dir, "proxy.db.usage-journal.jsonl"), newTestLogger())
	if err != nil {
		tb.Fatal(err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{RequestTimeoutSeconds: 30},
		Targets: []config.Target{{
			Name: "persist-target", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL,
			APIKey: "sk-test-key", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}},
		}},
	}
	f.service, err = NewService(cfg, newTestLogger())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { f.service.Close() })
	if legacy {
		db.MaxBatchDelay = 10 * time.Millisecond
		f.service.SetRequestStatsRecorder(f.stats)
	} else {
		if err := f.journal.EnableAsyncCommit(nosql.UsageJournalAsyncOptions{}); err != nil {
			tb.Fatal(err)
		}
		f.async = nosql.NewAsyncRequestStatsRecorder(f.stats, nosql.AsyncRequestStatsOptions{}, newTestLogger())
		f.service.SetRequestStatsRecorder(f.async)
		tb.Cleanup(func() { f.journal.Close(); f.async.Close() })
	}
	f.service.SetUsageRecorder(f.journal)
	principal := persistPrincipal(tb)
	f.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	tb.Cleanup(f.front.Close)
	return f
}

func persistPrincipal(tb testing.TB) *auth.Principal {
	tb.Helper()
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "persist-token")); err != nil {
		tb.Fatal(err)
	}
	principal, ok := store.Authenticate("persist-token")
	if !ok {
		tb.Fatal("expected principal")
	}
	return principal
}

// streamDoneToEOF sends one streaming request and returns the time between
// the client reading the [DONE] line and the response reaching EOF.
func (f *persistenceFixture) streamDoneToEOF(client *http.Client) (time.Duration, error) {
	req, err := http.NewRequest(http.MethodPost, f.front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) == "data: [DONE]" {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("stream ended before [DONE]: %w", err)
		}
	}
	done := time.Now()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return 0, err
	}
	return time.Since(done), nil
}

// Persisting usage and completion statistics must not sit between the
// stream's [DONE] and its end: with the database write lock held for the
// whole test, every response still ends promptly, and Close later writes
// every acknowledged event.
func TestStreamDoneToEOFNotBlockedByDatabaseCommit(t *testing.T) {
	f := newPersistenceFixture(t, false)
	tx, err := f.db.Begin(true) // stalls every bbolt write
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	const requests = 8
	var delays []time.Duration
	for i := 0; i < requests; i++ {
		delay, err := f.streamDoneToEOF(client)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("request %d: %v", i, err)
		}
		delays = append(delays, delay)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	t.Logf("[DONE]->EOF with stalled database: p50=%v max=%v", delays[len(delays)/2], delays[len(delays)-1])
	// Only the journal fsync remains on the response path.
	if limit := time.Second; delays[len(delays)/2] > limit {
		t.Fatalf("[DONE]->EOF p50 %v waited for the stalled database (limit %v)", delays[len(delays)/2], limit)
	}
	if err := f.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.async.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.List(usage.Filter{ClientName: "tester", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != requests {
		t.Fatalf("usage events after Close = %d, want %d", len(events), requests)
	}
	for _, event := range events {
		if event.InputTokens != 7 || event.OutputTokens != 3 {
			t.Fatalf("usage mismatch: %+v", event)
		}
	}
	snapshot, err := f.stats.Snapshot(time.Now().Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Total != requests || snapshot.Success != requests {
		t.Fatalf("completions after Close = %+v, want %d successes", snapshot.Counters, requests)
	}
	if f.service.RequestStatsRecordFailures() != 0 || f.service.MetricsSnapshot().TotalUsageRecordFailures != 0 {
		t.Fatalf("unexpected record failures: stats=%d usage=%d", f.service.RequestStatsRecordFailures(), f.service.MetricsSnapshot().TotalUsageRecordFailures)
	}
}

// Asynchronous statistics write failures keep the existing record_failures
// signal and never change the response.
func TestAsyncRequestStatsWriteFailureCountsAsRecordFailure(t *testing.T) {
	f := newPersistenceFixture(t, false)
	// Corrupt the current minute so the asynchronous write fails.
	if err := f.db.Update(func(tx *bolt.Tx) error {
		for _, at := range []time.Time{time.Now(), time.Now().Add(time.Minute)} {
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, uint64(at.UTC().Unix()/60))
			if err := tx.Bucket([]byte(nosql.BucketRequestStats)).Put(key, []byte("{")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.streamDoneToEOF(&http.Client{Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	f.async.Flush()
	if got := f.service.RequestStatsRecordFailures(); got != 1 {
		t.Fatalf("RequestStatsRecordFailures = %d, want 1", got)
	}
	f.journal.Flush()
	if events, err := f.store.List(usage.Filter{ClientName: "tester"}); err != nil || len(events) != 1 {
		t.Fatalf("usage must be unaffected by stats failure: %d err=%v", len(events), err)
	}
}

// A full statistics queue drops the completion with the existing failure
// signal instead of blocking the response.
func TestRequestStatsQueueFullIsRecordFailure(t *testing.T) {
	service, err := NewService(&config.Config{}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetRequestStatsRecorder(failingStatsRecorder{err: nosql.ErrRequestStatsQueueFull})
	service.recordRequestCompletion(service.currentRequestStatsRecorder(), requeststats.Success)
	if service.RequestStatsRecordFailures() != 1 {
		t.Fatalf("queue-full drop not counted: %d", service.RequestStatsRecordFailures())
	}
}

type failingStatsRecorder struct{ err error }

func (r failingStatsRecorder) Record(requeststats.Event) error { return r.err }

// BenchmarkStreamDoneToEOF measures the client-visible delay between [DONE]
// and EOF for sequential (low-concurrency) streaming requests.
//
//	go test ./internal/proxy -run '^$' -bench StreamDoneToEOF -benchtime 200x
func BenchmarkStreamDoneToEOF(b *testing.B) {
	for _, mode := range []struct {
		name   string
		legacy bool
	}{{"legacy_sync_10ms", true}, {"async_journal_2ms", false}} {
		b.Run(mode.name, func(b *testing.B) {
			f := newPersistenceFixture(b, mode.legacy)
			client := &http.Client{Timeout: 10 * time.Second}
			delays := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				delay, err := f.streamDoneToEOF(client)
				if err != nil {
					b.Fatal(err)
				}
				delays = append(delays, delay)
			}
			b.StopTimer()
			sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
			b.ReportMetric(float64(delays[len(delays)/2].Microseconds())/1000, "p50_ms")
			b.ReportMetric(float64(delays[(len(delays)*99)/100].Microseconds())/1000, "p99_ms")
		})
	}
}
