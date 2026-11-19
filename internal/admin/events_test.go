package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
)

// eventsTestEnv mounts the admin handler exactly like cmd/proxy does (security
// headers + session middleware under /admin) and shortens the hub timings so a
// test never waits for the real 5s/15s/10m ticks. Timings are applied before
// httptest.NewServer starts the serving goroutine, so -race sees them ordered.
type eventsTestEnv struct {
	handler        *Handler
	sessions       *SessionManager
	service        *proxy.Service
	server         *httptest.Server
	client         *http.Client
	readonlyCookie *http.Cookie
}

func newEventsTestEnv(t *testing.T, configure func(*Handler)) *eventsTestEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(dir, 2, nil)
	path := filepath.Join(dir, "config.json")
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	sessions := NewSessionManager(config.AdminSessionConfig{
		Secret: "events-test-secret", CookieName: "events_session", TTLSeconds: 3600,
	}, logger)
	h := NewHandler(config.NewManager(path), auth.NewStore(), service, stores.auditStore, stores.userStore,
		stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessions, logger)

	// Quiet defaults: no broadcast ticks or heartbeats unless a test asks for
	// them; every connection has a short bounded lifetime.
	h.events.interval = time.Hour
	h.events.heartbeat = time.Hour
	h.events.maxLifetime = 5 * time.Second
	h.events.writeTimeout = time.Second
	if configure != nil {
		configure(h)
	}

	router := chi.NewRouter()
	router.Route("/admin", func(r chi.Router) {
		r.Use(AdminSecurityHeaders)
		r.Use(sessions.Middleware)
		r.Mount("/", h)
	})
	server := httptest.NewServer(router)
	// LIFO cleanup: stop streams, close the server, then close the service;
	// store and DB cleanups registered earlier run last.
	t.Cleanup(h.StopEvents)
	t.Cleanup(server.Close)

	_, cookie, err := sessions.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("readonly session: %v", err)
	}
	return &eventsTestEnv{
		handler:        h,
		sessions:       sessions,
		service:        service,
		server:         server,
		client:         &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		readonlyCookie: cookie,
	}
}

// openStream opens an authenticated SSE connection and returns the response
// and a buffered reader over its body.
func (e *eventsTestEnv) openStream(t *testing.T, ctx context.Context) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.server.URL+"/admin/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(e.readonlyCookie)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /admin/events: HTTP %d %s", resp.StatusCode, body)
	}
	return resp, bufio.NewReader(resp.Body)
}

type sseFrame struct {
	event string
	data  string
}

// readSSEFrameRaw reads one SSE frame, skipping comments (": ping") and the
// retry hint. It returns the underlying read error, including EOF.
func readSSEFrameRaw(br *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if frame.event != "" || frame.data != "" {
				return frame, nil
			}
		case strings.HasPrefix(line, ":"):
			// heartbeat comment
		case strings.HasPrefix(line, "event:"):
			frame.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			frame.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func readSnapshotFrame(t *testing.T, br *bufio.Reader) (map[string]any, string) {
	t.Helper()
	for {
		frame, err := readSSEFrameRaw(br)
		if err != nil {
			t.Fatalf("read snapshot frame: %v", err)
		}
		if frame.event != "snapshot" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(frame.data), &body); err != nil {
			t.Fatalf("decode snapshot %q: %v", frame.data, err)
		}
		return body, frame.data
	}
}

func eventsSubscriberCount(h *Handler) int {
	h.events.mu.Lock()
	defer h.events.mu.Unlock()
	return len(h.events.subs)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Unauthenticated requests never reach the stream handler.
func TestEventsUnauthenticatedIs401(t *testing.T) {
	env := newEventsTestEnv(t, nil)
	resp, err := env.client.Get(env.server.URL + "/admin/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := eventsSubscriberCount(env.handler); got != 0 {
		t.Fatalf("subscribers = %d, want 0", got)
	}
}

// A readonly session may subscribe, and the first frame is a complete runtime
// snapshot with exactly the same targets_runtime as /admin/metrics.
func TestEventsReadonlyFirstFrameMatchesMetrics(t *testing.T) {
	env := newEventsTestEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, br := env.openStream(t, ctx)

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (AdminSecurityHeaders)", got)
	}

	// Stream prologue: EventSource reconnect hint, then the first event.
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read retry line: %v", err)
	}
	if got := strings.TrimRight(line, "\r\n"); got != "retry: 3000" {
		t.Fatalf("first line = %q, want retry: 3000", got)
	}
	if line, err = br.ReadString('\n'); err != nil || strings.TrimRight(line, "\r\n") != "" {
		t.Fatalf("retry block terminator = %q, err %v", line, err)
	}

	frame, raw := readSnapshotFrame(t, br)
	generatedAt, ok := frame["generated_at"].(string)
	if !ok {
		t.Fatalf("generated_at = %v", frame["generated_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, generatedAt); err != nil {
		t.Fatalf("generated_at %q is not RFC3339Nano: %v", generatedAt, err)
	}
	if len(frame) != 2 {
		t.Fatalf("snapshot keys = %v, want only generated_at and targets_runtime", frame)
	}
	runtimeEntries, ok := frame["targets_runtime"].([]any)
	if !ok || len(runtimeEntries) != 2 {
		t.Fatalf("targets_runtime = %v, want two targets", frame["targets_runtime"])
	}
	for _, entry := range runtimeEntries {
		target := entry.(map[string]any)
		for _, key := range []string{"window_15m", "model_cooldowns", "phase", "in_flight", "name"} {
			if _, ok := target[key]; !ok {
				t.Fatalf("targets_runtime entry missing %q: %v", key, target)
			}
		}
		if _, ok := target["endpoint"]; ok {
			t.Fatalf("targets_runtime must not expose endpoint: %v", target)
		}
	}
	if strings.Contains(raw, "example.com") || strings.Contains(raw, "api_key") {
		t.Fatalf("snapshot leaks endpoint or keys: %s", raw)
	}

	// Same source, same shape: read /admin/metrics with the same readonly
	// session and compare the block byte-for-byte at the JSON value level.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.server.URL+"/admin/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(env.readonlyCookie)
	metricsResp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/metrics: %v", err)
	}
	defer metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/metrics: HTTP %d", metricsResp.StatusCode)
	}
	var metrics map[string]any
	if err := json.NewDecoder(metricsResp.Body).Decode(&metrics); err != nil {
		t.Fatalf("decode /admin/metrics: %v", err)
	}
	if !reflect.DeepEqual(frame["targets_runtime"], metrics["targets_runtime"]) {
		t.Fatalf("/admin/events targets_runtime diverged from /admin/metrics:\nstream: %v\nmetrics: %v",
			frame["targets_runtime"], metrics["targets_runtime"])
	}
	_, streamShadow := frame["shadow_routing"]
	_, metricsShadow := metrics["shadow_routing"]
	if streamShadow != metricsShadow {
		t.Fatalf("shadow_routing presence: stream=%v metrics=%v (no statistics yet, both must omit it)",
			streamShadow, metricsShadow)
	}
}

// One hub tick builds and broadcasts the same serialized frame to every
// subscriber, including a fresh readonly one.
func TestEventsSubscribersShareOneTickFrame(t *testing.T) {
	hub := newEventsHub(func() []byte { return []byte(`{"tick":1}`) }, nil)
	hub.interval = time.Hour
	first, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer hub.stop()

	payload := []byte(`{"generated_at":"2026-11-19T00:00:00Z","targets_runtime":[]}`)
	hub.publish(payload)
	gotFirst := <-first.ch
	gotSecond := <-second.ch
	if string(gotFirst) != string(payload) || string(gotSecond) != string(payload) {
		t.Fatalf("one tick delivered different frames: first=%s second=%s want=%s",
			gotFirst, gotSecond, payload)
	}
	if !reflect.DeepEqual(gotFirst, gotSecond) {
		t.Fatalf("subscribers received different bytes for one tick: %s vs %s", gotFirst, gotSecond)
	}
}

// A slow subscriber must never block publishing or other subscribers; its
// channel keeps only the latest frame.
func TestEventsHubSlowSubscriberDoesNotBlock(t *testing.T) {
	hub := newEventsHub(func() []byte { return []byte(`{}`) }, nil)
	hub.interval = time.Hour
	slow, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	fast, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer hub.stop()

	start := time.Now()
	for i := 0; i < 1000; i++ {
		hub.publish([]byte(fmt.Sprintf(`{"n":%d}`, i)))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("publishing blocked on a slow subscriber: %v", elapsed)
	}

	select {
	case data := <-slow.ch:
		if string(data) != `{"n":999}` {
			t.Fatalf("slow subscriber pending frame = %s, want latest", data)
		}
	default:
		t.Fatal("slow subscriber has no pending frame")
	}
	// Capacity 1 + latest-wins: nothing else queued behind it.
	select {
	case data := <-slow.ch:
		t.Fatalf("slow subscriber queued an extra frame: %s", data)
	default:
	}
	select {
	case <-fast.ch:
	default:
		t.Fatal("active subscriber missed the broadcast")
	}
}

// Over the global subscriber limit the handler answers 503 + Retry-After
// before writing any stream headers.
func TestEventsSubscriberLimitReturns503(t *testing.T) {
	env := newEventsTestEnv(t, func(h *Handler) { h.events.maxSubscribers = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, br := env.openStream(t, ctx)
	readSnapshotFrame(t, br)

	// The env client keeps no cookie jar; replay the authenticated cookie so
	// the request reaches the subscriber limit instead of the 401 short-circuit.
	req, err := http.NewRequest(http.MethodGet, env.server.URL+"/admin/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(env.readonlyCookie)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON rejection", ct)
	}
	if got := eventsSubscriberCount(env.handler); got != 1 {
		t.Fatalf("subscribers = %d, want 1 (rejection must not subscribe)", got)
	}

	// A freed slot admits the next stream.
	cancel()
	waitFor(t, 3*time.Second, "subscriber to leave", func() bool { return eventsSubscriberCount(env.handler) == 0 })
}

// Canceling the request context ends the stream and unsubscribes.
func TestEventsRequestContextCancelEndsStream(t *testing.T) {
	env := newEventsTestEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	_, br := env.openStream(t, ctx)
	readSnapshotFrame(t, br)
	if got := eventsSubscriberCount(env.handler); got != 1 {
		t.Fatalf("subscribers = %d, want 1", got)
	}

	cancel()
	for {
		if _, err := readSSEFrameRaw(br); err != nil {
			break
		}
	}
	waitFor(t, 3*time.Second, "handler to unsubscribe after cancel", func() bool {
		return eventsSubscriberCount(env.handler) == 0
	})
}

// The bounded connection lifetime ends the stream so EventSource reconnects
// and revalidates the session.
func TestEventsMaxLifetimeEndsStream(t *testing.T) {
	env := newEventsTestEnv(t, func(h *Handler) { h.events.maxLifetime = 80 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, br := env.openStream(t, ctx)
	readSnapshotFrame(t, br)

	if _, err := readSSEFrameRaw(br); err == nil {
		t.Fatal("stream still open after max lifetime")
	}
	waitFor(t, 3*time.Second, "handler to unsubscribe after lifetime", func() bool {
		return eventsSubscriberCount(env.handler) == 0
	})
}

// Heartbeats keep intermediaries from idling out the stream.
func TestEventsHeartbeatPing(t *testing.T) {
	env := newEventsTestEnv(t, func(h *Handler) { h.events.heartbeat = 20 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, br := env.openStream(t, ctx)
	readSnapshotFrame(t, br)

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read heartbeat: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == ": ping" {
			return
		}
	}
}

// StopEvents closes every live stream, stops the hub goroutine, and is safe to
// call again (server shutdown may race with test cleanup).
func TestEventsStopEventsClosesStreamsAndStopsHub(t *testing.T) {
	env := newEventsTestEnv(t, nil)
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readers := make([]*bufio.Reader, 0, 2)
	for i := 0; i < 2; i++ {
		_, br := env.openStream(t, ctx)
		readSnapshotFrame(t, br)
		readers = append(readers, br)
	}
	if got := eventsSubscriberCount(env.handler); got != 2 {
		t.Fatalf("subscribers = %d, want 2", got)
	}

	env.handler.StopEvents()
	if got := eventsSubscriberCount(env.handler); got != 0 {
		t.Fatalf("subscribers after StopEvents = %d, want 0", got)
	}
	for _, br := range readers {
		for {
			if _, err := readSSEFrameRaw(br); err != nil {
				break
			}
		}
	}
	env.handler.StopEvents() // repeated call must not panic or block

	waitFor(t, 3*time.Second, "hub goroutine and connections to exit", func() bool {
		return runtime.NumGoroutine() <= base
	})
}

// An idle hub is zero-overhead: no goroutine and no snapshot work until the
// first subscriber.
func TestEventsHubIsLazyWithoutSubscribers(t *testing.T) {
	var builds atomic.Int32
	hub := newEventsHub(func() []byte {
		builds.Add(1)
		return []byte(`{"ok":true}`)
	}, nil)
	hub.interval = 10 * time.Millisecond
	defer hub.stop()

	time.Sleep(50 * time.Millisecond)
	if n := builds.Load(); n != 0 {
		t.Fatalf("hub built %d snapshots without subscribers", n)
	}

	sub, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	if data := hub.snapshot(); string(data) != `{"ok":true}` {
		t.Fatalf("first snapshot = %s", data)
	}
	waitFor(t, 3*time.Second, "hub tick", func() bool { return builds.Load() >= 2 })

	hub.unsubscribe(sub)
	time.Sleep(30 * time.Millisecond) // let an in-flight tick settle
	settled := builds.Load()
	time.Sleep(60 * time.Millisecond)
	if n := builds.Load(); n != settled {
		t.Fatalf("hub kept building after the last subscriber left: %d -> %d", settled, n)
	}
}
