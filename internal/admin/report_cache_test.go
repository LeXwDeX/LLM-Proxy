package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newCountingHandler(clock *fakeClock) (*Handler, *atomic.Int64) {
	cache := newReportCache(5 * time.Second)
	cache.now = clock.Now
	return &Handler{reportCache: cache}, new(atomic.Int64)
}

func serveReport(h http.HandlerFunc, target string, session *Session) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if session != nil {
		req = req.WithContext(ContextWithSession(req.Context(), session))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestReportCacheCollapsesConcurrentIdenticalRequests(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h, reads := newCountingHandler(clock)
	release := make(chan struct{})
	wrapped := h.cachedReport(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		<-release
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})

	const callers = 12
	var wg sync.WaitGroup
	bodies := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bodies[i] = serveReport(wrapped, "/data/usage/aggregate?from=a", nil).Body.String()
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let every caller join the flight
	close(release)
	wg.Wait()
	if got := reads.Load(); got != 1 {
		t.Fatalf("underlying reads = %d, want 1 for %d concurrent identical requests", got, callers)
	}
	for i, b := range bodies {
		if !strings.Contains(b, `"ok":"1"`) {
			t.Fatalf("caller %d body = %q", i, b)
		}
	}
}

func TestReportCacheTTLExpiryRereads(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h, reads := newCountingHandler(clock)
	wrapped := h.cachedReport(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int64{"n": reads.Add(1)})
	})
	first := serveReport(wrapped, "/x", nil).Body.String()
	clock.Advance(4 * time.Second)
	if got := serveReport(wrapped, "/x", nil).Body.String(); got != first || reads.Load() != 1 {
		t.Fatalf("within TTL: body %q (want %q), reads %d (want 1)", got, first, reads.Load())
	}
	clock.Advance(2 * time.Second)
	second := serveReport(wrapped, "/x", nil).Body.String()
	if reads.Load() != 2 || second == first {
		t.Fatalf("after TTL: body %q, reads %d, want a fresh read", second, reads.Load())
	}
}

func TestReportCacheSeparatesParametersAndRoles(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h, reads := newCountingHandler(clock)
	wrapped := h.cachedReport(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		role := "none"
		if s, ok := SessionFromContext(r.Context()); ok {
			role = s.Role
		}
		writeJSON(w, http.StatusOK, map[string]string{"role": role, "q": r.URL.RawQuery})
	})
	admin := &Session{Username: "a", Role: RoleAdmin}
	ro := &Session{Username: "b", Role: RoleReadonly}

	adminBody := serveReport(wrapped, "/x?client_name=a", admin).Body.String()
	roBody := serveReport(wrapped, "/x?client_name=a", ro).Body.String()
	if adminBody == roBody || !strings.Contains(roBody, RoleReadonly) {
		t.Fatalf("roles shared a cache entry: admin=%q readonly=%q", adminBody, roBody)
	}
	if body := serveReport(wrapped, "/x?client_name=b", admin).Body.String(); !strings.Contains(body, "client_name=b") {
		t.Fatalf("different params shared an entry: %q", body)
	}
	if body := serveReport(wrapped, "/x?model_group=g1&model_group=g2", admin).Body.String(); !strings.Contains(body, "g1&model_group=g2") {
		t.Fatalf("repeated params body = %q", body)
	}
	if body := serveReport(wrapped, "/x?model_group=g2&model_group=g1", admin).Body.String(); !strings.Contains(body, "g2&model_group=g1") {
		t.Fatalf("param order shared an entry: %q", body)
	}
	if reads.Load() != 5 {
		t.Fatalf("reads = %d, want 5 distinct keys", reads.Load())
	}
	// Same key again hits.
	serveReport(wrapped, "/x?client_name=a", ro)
	if reads.Load() != 5 {
		t.Fatalf("repeat of a cached key re-read: %d", reads.Load())
	}
}

func TestReportCacheDoesNotCacheErrors(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h, reads := newCountingHandler(clock)
	wrapped := h.cachedReport(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			writeJSON(w, http.StatusInternalServerError, errorResponse("boom"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})
	if rec := serveReport(wrapped, "/x", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d", rec.Code)
	}
	if rec := serveReport(wrapped, "/x", nil); rec.Code != http.StatusOK || reads.Load() != 2 {
		t.Fatalf("error was cached: status %d reads %d", rec.Code, reads.Load())
	}
}

// The real routes use the cache: usage written inside the TTL is not shown
// (display-only staleness), and shows up once the entry expires.
func TestOverviewAndAggregateRoutesUseShortTTLCache(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(config.NewManager(configPath), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore,
		stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h.reportCache.now = clock.Now
	router := chi.NewRouter()
	router.Mount("/", h)

	overview := func() int64 {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/api/overview", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("overview %d %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Requests72h int64 `json:"requests_72h"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Requests72h
	}
	aggregate := func() string {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/aggregate", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("aggregate %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	if got := overview(); got != 0 {
		t.Fatalf("initial requests_72h = %d", got)
	}
	aggBefore := aggregate()
	if err := stores.usageStore.Record(usage.Event{
		Timestamp: time.Now().UTC().Add(-time.Minute), ClientName: "alice", Model: "m",
		EndpointType: "openai", Target: "upstream", StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if got := overview(); got != 0 {
		t.Fatalf("overview inside TTL = %d, want the cached 0", got)
	}
	if aggregate() != aggBefore {
		t.Fatal("aggregate inside TTL changed")
	}
	clock.Advance(defaultReportCacheTTL + time.Second)
	if got := overview(); got != 1 {
		t.Fatalf("overview after TTL = %d, want 1", got)
	}
	if aggregate() == aggBefore {
		t.Fatal("aggregate did not refresh after TTL")
	}
}

// A leader whose client goes away must not fail the callers sharing its flight.
func TestReportCacheLeaderAbortDoesNotFailWaiters(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h, reads := newCountingHandler(clock)
	leaderIn := make(chan struct{})
	wrapped := h.cachedReport(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			close(leaderIn)
			<-r.Context().Done() // leader's scan stops when its client leaves
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(leaderCtx)
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		leaderDone <- rec
	}()
	<-leaderIn
	waiterDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiterDone <- serveReport(wrapped, "/x", nil) }()
	time.Sleep(100 * time.Millisecond) // waiter joins the leader's flight
	cancelLeader()

	if rec := <-leaderDone; rec.Body.Len() != 0 {
		t.Fatalf("aborted leader wrote %q", rec.Body.String())
	}
	select {
	case rec := <-waiterDone:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":"1"`) {
			t.Fatalf("waiter got %d %q after leader abort", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter hung after leader abort")
	}
}
