package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

func wsTwoKeyService(t *testing.T, endpoint string) *Service {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{
		Name: "openai", EndpointType: "openai", Endpoint: endpoint, APIKey: "prewarm-key-a", APIKeys: []string{"prewarm-key-b"},
		PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
	}}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// keyIndexOf maps the credential a handshake used to its key pool index.
func (u *wsPrewarmUpstream) keyIndexOf(t *testing.T, attempt int32) int {
	t.Helper()
	u.mu.Lock()
	auth := u.authOf[attempt]
	u.mu.Unlock()
	switch {
	case strings.HasSuffix(auth, "prewarm-key-a"):
		return 0
	case strings.HasSuffix(auth, "prewarm-key-b"):
		return 1
	}
	t.Fatalf("handshake %d used unknown credential", attempt)
	return -1
}

func wsSpareLanes(s *Service) (spares, prewarming int) {
	s.responsesWS.mu.Lock()
	defer s.responsesWS.mu.Unlock()
	for e := range s.responsesWS.entries {
		if e.prewarmed {
			spares++
		}
		if e.prewarming {
			prewarming++
		}
	}
	return spares, prewarming
}

// A background handshake that hangs must not hold up the client's next fresh
// request: it waits for the spare only briefly and then dials itself.
func TestResponsesWSHungSpareDoesNotDelayNextRequest(t *testing.T) {
	u := &wsPrewarmUpstream{hold: make(chan struct{}), holdFrom: 2, holdUntil: 2}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	defer close(u.hold)
	s := newWSTestService(t, server.URL, true)
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	waitForWS(t, "the hanging background handshake", func() bool { return u.attempts.Load() == 2 })
	started := time.Now()
	w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody)
	if elapsed := time.Since(started); w.Code != 200 || elapsed > time.Second {
		t.Fatalf("next request waited on the hung spare: status=%d elapsed=%v body=%s", w.Code, elapsed, w.Body.String())
	}
	if m := s.MetricsSnapshot().ResponsesWS; m.NewHandshake != 2 || m.PrewarmUsed != 0 {
		t.Fatalf("metrics: %+v", m)
	}
}

// Blocking a key invalidates the pool; an in-flight background handshake with
// that key is canceled rather than completed and discarded.
func TestResponsesWSInvalidateCancelsInflightSpare(t *testing.T) {
	u := &wsPrewarmUpstream{hold: make(chan struct{}), holdFrom: 2, holdUntil: 2}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	defer close(u.hold)
	s := wsTwoKeyService(t, server.URL)
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	waitForWS(t, "the held background handshake", func() bool { return u.attempts.Load() == 2 })
	if err := s.BlockKey("openai", u.keyIndexOf(t, 2)); err != nil {
		t.Fatal(err)
	}
	waitForWS(t, "the background dial to be canceled", func() bool { return u.heldCanceled.Load() == 1 })
	waitForWS(t, "the canceled spare to be counted", func() bool { return s.MetricsSnapshot().ResponsesWS.PrewarmDiscarded == 1 })
	if m := s.MetricsSnapshot().ResponsesWS; m.PrewarmDialOK != 0 || m.PrewarmDialFail != 0 {
		t.Fatalf("canceled spare settled as a dial outcome: %+v", m)
	}
}

// When the key pool takes a credential out of rotation, idle spares opened
// with it are retired at once instead of idling and being pinged.
func TestResponsesWSExhaustedKeyRetiresIdleSpare(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := wsTwoKeyService(t, server.URL)
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	waitForWS(t, "the spare lane", func() bool { spares, _ := wsSpareLanes(s); return spares == 1 })
	state, _ := s.targetByName("openai")
	state.keyPool.markExhaustedWithError(u.keyIndexOf(t, 2), "rate_limited", "")
	waitForWS(t, "the spare to be retired", func() bool { spares, _ := wsSpareLanes(s); return spares == 0 })
	// The lane holding resp_1 stays continuable; only the spare socket closes.
	waitForWS(t, "the spare socket to close", func() bool { return u.active.Load() == 1 })
	s.responsesWS.mu.Lock()
	lanes := len(s.responsesWS.entries)
	s.responsesWS.mu.Unlock()
	if lanes != 1 {
		t.Fatalf("lanes=%d, want the conversation lane only", lanes)
	}
}

// A placeholder for a background dial must not push a foreground request to
// HTTP: the request needing the slot cancels the dial and takes its place.
func TestResponsesWSForegroundTakesSlotFromInflightSpare(t *testing.T) {
	for _, scope := range []string{"client", "global"} {
		t.Run(scope, func(t *testing.T) {
			u := &wsPrewarmUpstream{}
			server := httptest.NewServer(http.HandlerFunc(u.serve))
			defer server.Close()
			p := newResponsesWSPool()
			defer p.close()
			owner := [32]byte{1}
			now := time.Now()
			var canceled atomic.Bool
			spareOwner := owner
			if scope == "global" {
				spareOwner = [32]byte{2}
			}
			placeholder := &responsesWSConn{owner: spareOwner, key: [32]byte{3}, created: now, used: now, busy: true,
				prewarming: true, ready: make(chan struct{}), cancelDial: func() { canceled.Store(true) }}
			p.entries[placeholder] = struct{}{}
			busy := responsesWSPerClient - 1
			if scope == "global" {
				busy = responsesWSMax - 1
			}
			for i := 0; i < busy; i++ {
				e := &responsesWSConn{owner: owner, created: now, used: now, busy: true}
				if scope == "global" {
					e.owner = [32]byte{3, byte(i)}
				}
				p.entries[e] = struct{}{}
			}
			e, resp, fallback, err := p.acquire(context.Background(), nil, wsLockTestRequest(t, server.URL), owner, "")
			if err != nil || resp != nil || fallback || e == nil {
				t.Fatalf("foreground request lost its slot: conn=%v resp=%v fallback=%v err=%v", e, resp, fallback, err)
			}
			p.release(e, false)
			p.mu.Lock()
			_, kept := p.entries[placeholder]
			p.mu.Unlock()
			if kept || !canceled.Load() || p.stats.fallbackCap.Load() != 0 {
				t.Fatalf("spare placeholder kept=%v canceled=%v fallbackCap=%d", kept, canceled.Load(), p.stats.fallbackCap.Load())
			}
		})
	}
}

func TestResponsesWSReapPurgesExpiredMemories(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	now := time.Now()
	p.prewarmBackoff = map[[32]byte]time.Time{{1}: now.Add(-time.Second), {2}: now.Add(time.Minute)}
	p.unsupported = map[[32]byte]time.Time{{3}: now.Add(-time.Second), {4}: now.Add(time.Minute)}
	p.reapAt(now)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.prewarmBackoff[[32]byte{2}]; len(p.prewarmBackoff) != 1 || !ok {
		t.Fatalf("backoff after reap: %v", p.prewarmBackoff)
	}
	if _, ok := p.unsupported[[32]byte{4}]; len(p.unsupported) != 1 || !ok {
		t.Fatalf("unsupported after reap: %v", p.unsupported)
	}
}

// The background dial uses the same handshake bound as a foreground one: the
// client timeout when it is below five seconds.
func TestResponsesWSSpareDialHonorsClientTimeout(t *testing.T) {
	u := &wsPrewarmUpstream{hold: make(chan struct{}), holdFrom: 2, holdUntil: 2}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	defer close(u.hold)
	s := newWSTestService(t, server.URL, true)
	s.httpClient.Timeout = 300 * time.Millisecond
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	started := time.Now()
	waitForWS(t, "the background dial to time out", func() bool { return s.MetricsSnapshot().ResponsesWS.PrewarmDialFail == 1 })
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("background dial ignored the client timeout: %v", elapsed)
	}
}

func waitWSPrewarmSettled(t *testing.T, s *Service) {
	t.Helper()
	waitForWS(t, "background dials to settle", func() bool { _, prewarming := wsSpareLanes(s); return prewarming == 0 })
}

// Prewarm-enabled counterparts of the pooled cases that pin exact handshake
// counts with prewarm off: once background dials settle, every upstream
// handshake is either a foreground one or a counted spare, continuations stay
// on their socket and each request settles exactly one usage event.
func TestResponsesWSPrewarmSettledCounts(t *testing.T) {
	t.Run("cross_client_isolation", func(t *testing.T) {
		s, rec, u := newWSLifecycleHarness(t, wsModeComplete)
		if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`); w.Code != 200 {
			t.Fatalf("first: %d %s", w.Code, w.Body.String())
		}
		w := wsTestRequest(t, s, "client-b", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true,"previous_response_id":"resp_1"}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "previous_response_not_found") {
			t.Fatalf("cross-client previous: %d %s", w.Code, w.Body.String())
		}
		waitWSPrewarmSettled(t, s)
		wsRequireNoProtocolViolations(t, u, 1)
		m := s.MetricsSnapshot().ResponsesWS
		if u.upgrades.Load() != int32(m.NewHandshake+m.PrewarmDialOK) || m.NewHandshake != 1 || m.PrewarmDialOK != 1 || m.PrewarmUsed != 0 {
			t.Fatalf("upgrades=%d metrics=%+v", u.upgrades.Load(), m)
		}
		if events := rec.snapshot(); len(events) != 1 {
			t.Fatalf("usage events=%d", len(events))
		}
	})
	t.Run("nonstream_final_with_usage", func(t *testing.T) {
		s, rec, u := newWSLifecycleHarness(t, wsModeComplete)
		if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hi"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"resp_1"`) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		waitWSPrewarmSettled(t, s)
		wsRequireNoProtocolViolations(t, u, 1)
		m := s.MetricsSnapshot().ResponsesWS
		if u.upgrades.Load() != 2 || m.NewHandshake != 1 || m.PrewarmDialOK != 1 {
			t.Fatalf("upgrades=%d metrics=%+v", u.upgrades.Load(), m)
		}
		if events := rec.snapshot(); len(events) != 1 || events[0].InputTokens != 8 || events[0].OutputTokens != 5 {
			t.Fatalf("usage events: %+v", events)
		}
	})
	t.Run("continuation_same_socket", func(t *testing.T) {
		s, rec, u := newWSLifecycleHarness(t, wsModeComplete)
		for _, body := range []string{
			`{"model":"openai-test","input":"hi","stream":true}`,
			`{"model":"openai-test","input":"again","stream":true,"previous_response_id":"resp_1"}`,
			`{"model":"openai-test","input":"fresh","stream":true}`,
		} {
			if w := wsTestRequest(t, s, "client-a", "/v1/responses", body); w.Code != 200 {
				t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
			}
			waitWSPrewarmSettled(t, s)
		}
		wsRequireNoProtocolViolations(t, u, 3)
		previous := u.snapshotPreviousIDs()
		m := s.MetricsSnapshot().ResponsesWS
		if len(previous) != 3 || previous[0] != "" || previous[1] != "resp_1" || previous[2] != "" {
			t.Fatalf("previous=%q", previous)
		}
		if m.NewHandshake != 1 || m.HitContinuation != 1 || m.PrewarmUsed != 1 || u.upgrades.Load() != int32(m.NewHandshake+m.PrewarmDialOK) {
			t.Fatalf("upgrades=%d metrics=%+v", u.upgrades.Load(), m)
		}
		if events := rec.snapshot(); len(events) != 3 {
			t.Fatalf("usage events=%d", len(events))
		}
	})
	t.Run("reuse_and_accounting", func(t *testing.T) {
		s, rec, u := newWSLifecycleHarness(t, wsModeComplete)
		for i := 0; i < 2; i++ {
			if w := wsTestRequest(t, s, "client-a", "/v1/responses", `{"model":"openai-test","input":"hello","stream":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.completed\n") {
				t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
			}
			waitWSPrewarmSettled(t, s)
		}
		wsRequireNoProtocolViolations(t, u, 2)
		m := s.MetricsSnapshot().ResponsesWS
		if m.NewHandshake != 1 || m.PrewarmUsed != 1 || u.upgrades.Load() != int32(m.NewHandshake+m.PrewarmDialOK) || u.posts.Load() != 0 {
			t.Fatalf("upgrades=%d posts=%d metrics=%+v", u.upgrades.Load(), u.posts.Load(), m)
		}
		events := rec.snapshot()
		if len(events) != 2 || events[0].InputTokens != 8 || events[0].CachedTokens != 2 || events[0].OutputTokens != 5 || events[0].CostIncomplete {
			t.Fatalf("accounting: %+v", events)
		}
		if s.TargetStatuses(time.Now())[0].InFlight != 0 {
			t.Fatal("request capacity leaked")
		}
	})
	t.Run("key_failure_moves_to_healthy_key", func(t *testing.T) {
		var attempts, creates, posts atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			if r.Method == http.MethodPost {
				posts.Add(1)
				http.Error(w, "unexpected replay", 500)
				return
			}
			if strings.HasSuffix(r.Header.Get("Authorization"), "prewarm-key-a") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"rate limit"}}`))
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			for {
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				n := creates.Add(1)
				_ = c.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"healthy_%d","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`, n)))
			}
		}))
		defer upstream.Close()
		s := wsTwoKeyService(t, upstream.URL)
		rec := &syncWSRecorder{}
		s.SetUsageRecorder(rec)
		for i := 1; i <= 2; i++ {
			w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi","stream":true}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"id":"healthy_%d"`, i)) {
				t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
			}
			waitWSPrewarmSettled(t, s)
		}
		m := s.MetricsSnapshot().ResponsesWS
		// Handshakes: at most one rejected foreground attempt with key a, then
		// key b's foreground lane and its spare; no spare ever uses key a.
		if creates.Load() != 2 || posts.Load() != 0 || m.PrewarmDialFail != 0 || attempts.Load() != int32(m.NewHandshake+m.PrewarmDialOK) || m.PrewarmUsed != 1 {
			t.Fatalf("attempts=%d creates=%d posts=%d metrics=%+v", attempts.Load(), creates.Load(), posts.Load(), m)
		}
		if events := rec.snapshot(); len(events) != 2 {
			t.Fatalf("usage events=%d", len(events))
		}
	})
}
