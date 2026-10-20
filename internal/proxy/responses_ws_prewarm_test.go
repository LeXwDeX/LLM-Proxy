package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

// wsPrewarmUpstream is a WS-only mock whose handshakes can be rejected or held
// by attempt number. It never calls t.* from server goroutines.
type wsPrewarmUpstream struct {
	attempts, active, creates atomic.Int32
	reject                    func(attempt int32) int // non-zero: answer this handshake with that status
	hold                      chan struct{}           // when set, handshake attempts from holdFrom on wait here
	holdFrom                  int32
	holdUntil                 int32        // when non-zero, only attempts up to this one are held
	heldCanceled              atomic.Int32 // held handshakes the proxy abandoned
	mu                        sync.Mutex
	authOf                    map[int32]string // handshake attempt → Authorization
	producedOn                map[string]int32 // response id → handshake attempt that produced it
	continuedOn               map[string]int32 // previous_response_id → handshake attempt that continued it
}

func (u *wsPrewarmUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		http.Error(w, "HTTP was used", http.StatusInternalServerError)
		return
	}
	n := u.attempts.Add(1)
	u.mu.Lock()
	if u.authOf == nil {
		u.authOf = map[int32]string{}
	}
	u.authOf[n] = r.Header.Get("Authorization")
	u.mu.Unlock()
	if u.reject != nil {
		if status := u.reject(n); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided"}}`))
			return
		}
	}
	if u.hold != nil && n >= u.holdFrom && (u.holdUntil == 0 || n <= u.holdUntil) {
		select {
		case <-u.hold:
		case <-r.Context().Done():
			u.heldCanceled.Add(1)
			return
		}
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	u.active.Add(1)
	defer u.active.Add(-1)
	defer c.CloseNow()
	for {
		_, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var create struct {
			Previous string `json:"previous_response_id"`
		}
		_ = json.Unmarshal(msg, &create)
		id := fmt.Sprintf("resp_%d", u.creates.Add(1))
		u.mu.Lock()
		if u.producedOn == nil {
			u.producedOn, u.continuedOn = map[string]int32{}, map[string]int32{}
		}
		u.producedOn[id] = n
		if create.Previous != "" {
			u.continuedOn[create.Previous] = n
		}
		u.mu.Unlock()
		if c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"`+id+`","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)) != nil {
			return
		}
	}
}

func waitForWS(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const wsPrewarmBody = `{"model":"openai-test","input":"hi"}`

// Probe S1: one client sends 20 sequential fresh requests. Each finished turn
// keeps its response for a possible continuation, so without spare lanes the
// first four requests all pay a foreground cold handshake.
func TestResponsesWSPrewarmS1ForegroundHandshakes(t *testing.T) {
	for _, tc := range []struct {
		prewarm bool
		want    int64
	}{{false, 4}, {true, 1}} {
		t.Run(fmt.Sprint("prewarm=", tc.prewarm), func(t *testing.T) {
			u := &wsPrewarmUpstream{}
			server := httptest.NewServer(http.HandlerFunc(u.serve))
			defer server.Close()
			s := newWSTestService(t, server.URL, true)
			s.responsesWS.prewarm = tc.prewarm
			for i := 0; i < 20; i++ {
				if w := wsTestRequest(t, s, "client-s1", "/v1/responses", wsPrewarmBody); w.Code != 200 {
					t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			m := s.MetricsSnapshot().ResponsesWS
			if m.NewHandshake != tc.want || u.attempts.Load() > responsesWSPerClient {
				t.Fatalf("foreground handshakes=%d (want %d) upstream handshakes=%d metrics=%+v", m.NewHandshake, tc.want, u.attempts.Load(), m)
			}
			if hits := m.HitFresh + m.HitReuseCached + m.HitContinuation; hits+m.NewHandshake != 20 {
				t.Fatalf("lane outcomes do not add up: %+v", m)
			}
			if tc.prewarm && (m.PrewarmUsed != 3 || m.PrewarmDialOK != 3 || m.PrewarmDialFail != 0) {
				t.Fatalf("spare lanes: %+v", m)
			}
			if !tc.prewarm && m.PrewarmDialOK+m.PrewarmDialFail != 0 {
				t.Fatalf("disabled prewarm dialed: %+v", m)
			}
			// Foreground and background handshakes are timed separately.
			if m.HandshakeTCP.Count != m.NewHandshake || m.HandshakeUpgrade.Count != m.NewHandshake || m.HandshakeTLS.Count != 0 ||
				m.PrewarmHandshakeTCP.Count != m.PrewarmDialOK || m.PrewarmHandshakeUpgrade.Count != m.PrewarmDialOK {
				t.Fatalf("handshake phases: tcp=%+v upgrade=%+v tls=%+v prewarm tcp=%+v upgrade=%+v", m.HandshakeTCP, m.HandshakeUpgrade, m.HandshakeTLS, m.PrewarmHandshakeTCP, m.PrewarmHandshakeUpgrade)
			}
		})
	}
}

// A background dial is not a client request: whatever the upstream answers, it
// must not feed the key pool, the target breaker or the unsupported memory.
func TestResponsesWSPrewarmFailureIsIsolated(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			u := &wsPrewarmUpstream{reject: func(n int32) int {
				if n == 2 {
					return status
				}
				return 0
			}}
			server := httptest.NewServer(http.HandlerFunc(u.serve))
			defer server.Close()
			cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{
				Name: "openai", EndpointType: "openai", Endpoint: server.URL, APIKey: "upstream-test-key", APIKeys: []string{"upstream-second-key"},
				PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "openai-test"}},
			}}}
			s, err := NewService(cfg, newTestLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
				t.Fatalf("first: %d %s", w.Code, w.Body.String())
			}
			waitForWS(t, "the failed background dial", func() bool { return s.MetricsSnapshot().ResponsesWS.PrewarmDialFail == 1 })
			target := s.TargetStatuses(time.Now())[0]
			metrics := s.MetricsSnapshot()
			if target.Muted || target.ConsecutiveFailures != 0 || target.TotalFailedRequests != 0 || target.TotalKeys != 2 || target.ActiveKeys != 2 {
				t.Fatalf("background dial changed target state: %+v", target)
			}
			if metrics.TotalFailures != 0 || metrics.TotalKeyRetries != 0 || metrics.TotalTargetRetries != 0 || metrics.TotalRequests != 1 {
				t.Fatalf("background dial counted as a request: %+v", metrics)
			}
			s.responsesWS.mu.Lock()
			unsupported := len(s.responsesWS.unsupported)
			s.responsesWS.mu.Unlock()
			if unsupported != 0 {
				t.Fatal("background dial was remembered as an unsupported upstream")
			}
			// The failed key is backed off: the next turn's release must not dial
			// again for a minute, even though its lane holds a response.
			if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
				t.Fatalf("second: %d %s", w.Code, w.Body.String())
			}
			time.Sleep(100 * time.Millisecond)
			if m := s.MetricsSnapshot().ResponsesWS; m.PrewarmDialFail != 1 || m.PrewarmDialOK != 0 || u.attempts.Load() != 3 || m.NewHandshake != 2 {
				t.Fatalf("background dial not backed off: attempts=%d metrics=%+v", u.attempts.Load(), m)
			}
		})
	}
}

func TestResponsesWSPrewarmDiscardedOnConfigurationChange(t *testing.T) {
	u := &wsPrewarmUpstream{hold: make(chan struct{}), holdFrom: 2}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	waitForWS(t, "the background handshake", func() bool { return u.attempts.Load() == 2 })
	// What a target update calls; the in-flight spare belongs to the old generation.
	s.responsesWS.invalidate(false)
	close(u.hold)
	waitForWS(t, "the stale spare to be discarded", func() bool { return s.MetricsSnapshot().ResponsesWS.PrewarmDiscarded == 1 })
	waitForWS(t, "upstream sockets to close", func() bool { return u.active.Load() == 0 })
	s.responsesWS.mu.Lock()
	lanes := len(s.responsesWS.entries)
	s.responsesWS.mu.Unlock()
	if m := s.MetricsSnapshot().ResponsesWS; lanes != 0 || m.PrewarmDialOK != 0 {
		t.Fatalf("stale spare entered the pool: lanes=%d metrics=%+v", lanes, m)
	}
}

func TestResponsesWSPrewarmClosesWithoutLeaks(t *testing.T) {
	// The third handshake (the second spare) is held so Close races an
	// in-flight background dial.
	u := &wsPrewarmUpstream{hold: make(chan struct{}), holdFrom: 3}
	defer close(u.hold)
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	baseline := runtime.NumGoroutine()
	s := newWSTestService(t, server.URL, true)
	for i := 0; i < 2; i++ {
		if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	waitForWS(t, "the held background dial", func() bool { return u.attempts.Load() == 3 })
	if m := s.MetricsSnapshot().ResponsesWS; m.PrewarmUsed != 1 || m.NewHandshake != 1 {
		t.Fatalf("spare lane not used: %+v", m)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitForWS(t, "upstream sockets to close", func() bool { return u.active.Load() == 0 })
	waitForWS(t, "pool goroutines to exit", func() bool { return runtime.NumGoroutine() <= baseline })
	s.responsesWS.mu.Lock()
	lanes := len(s.responsesWS.entries)
	s.responsesWS.mu.Unlock()
	if lanes != 0 {
		t.Fatalf("lanes left after close: %d", lanes)
	}
}

// A continuation must reach the socket that produced its response; a spare
// lane has no lineage and may never stand in for it.
func TestResponsesWSPrewarmNeverServesContinuation(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	waitForWS(t, "the spare lane", func() bool { return s.MetricsSnapshot().ResponsesWS.PrewarmDialOK == 1 })
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"next","previous_response_id":"resp_1"}`)
	if w.Code != 200 {
		t.Fatalf("continuation: %d %s", w.Code, w.Body.String())
	}
	u.mu.Lock()
	produced, continued := u.producedOn["resp_1"], u.continuedOn["resp_1"]
	u.mu.Unlock()
	if produced != 1 || continued != 1 {
		t.Fatalf("continuation left its socket: produced on %d, continued on %d", produced, continued)
	}
	if w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"x","previous_response_id":"resp_unknown"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "previous_response_not_found") {
		t.Fatalf("unknown continuation: %d %s", w.Code, w.Body.String())
	}
	if m := s.MetricsSnapshot().ResponsesWS; m.PrewarmUsed != 0 || m.HitContinuation != 1 {
		t.Fatalf("spare lane served a continuation: %+v", m)
	}
}

// A client whose handshake identity changes per conversation keeps at most one
// spare, for its latest identity, and a needed lane evicts an unused spare
// before any lane that still holds a continuable response.
func TestResponsesWSPrewarmKeepsOneSparePerClient(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	principal := wsPrincipalFor(t, "client")
	send := func(session, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, wsReviewRequest(t, principal, body, map[string]string{"X-Session-ID": session}))
		return w
	}
	spares := func() (count int, keys map[[32]byte]bool) {
		s.responsesWS.mu.Lock()
		defer s.responsesWS.mu.Unlock()
		keys = map[[32]byte]bool{}
		for e := range s.responsesWS.entries {
			if e.prewarmed || e.prewarming {
				count++
				keys[e.key] = true
			}
		}
		return count, keys
	}
	for i := 1; i <= 3; i++ {
		if w := send(fmt.Sprint(i), wsPrewarmBody); w.Code != 200 {
			t.Fatalf("session %d: %d %s", i, w.Code, w.Body.String())
		}
		waitForWS(t, "the background dial to settle", func() bool {
			s.responsesWS.mu.Lock()
			defer s.responsesWS.mu.Unlock()
			for e := range s.responsesWS.entries {
				if e.prewarming {
					return false
				}
			}
			return true
		})
		if count, _ := spares(); count != 1 {
			t.Fatalf("after session %d: %d spare lanes, want 1", i, count)
		}
	}
	// Three conversation lanes and one spare fill the client's four lanes. A
	// fourth identity must evict the unused spare, not session 1's lane.
	if w := send("4", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("session 4: %d %s", w.Code, w.Body.String())
	}
	for i := 1; i <= 3; i++ {
		if w := send(fmt.Sprint(i), fmt.Sprintf(`{"model":"openai-test","input":"next","previous_response_id":"resp_%d"}`, i)); w.Code != 200 {
			t.Fatalf("continuation of session %d lost: %d %s", i, w.Code, w.Body.String())
		}
	}
}

// Header sampling compares identity headers of one client's adjacent
// handshakes and keeps only the names that changed, never their values.
func TestResponsesWSHandshakeHeaderChangeSample(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	s.responsesWS.prewarm = false
	owner := [32]byte{7}
	for i, auth := range []string{"Bearer secret-one", "Bearer secret-two"} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/responses", nil)
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Feature", "same")
		req.Header.Set("Traceparent", fmt.Sprint("trace-", i))
		e, resp, fallback, err := s.responsesWS.acquire(context.Background(), s.httpClient, req, owner, "")
		if err != nil || resp != nil || fallback || e == nil {
			t.Fatalf("acquire %d: conn=%v resp=%v fallback=%v err=%v", i, e, resp, fallback, err)
		}
		s.responsesWS.mu.Lock()
		e.responseID = "held"
		s.responsesWS.mu.Unlock()
		s.responsesWS.release(e, true)
	}
	m := s.MetricsSnapshot().ResponsesWS
	if m.HeaderChangeSamples != 1 || len(m.HeaderChanges) != 1 || m.HeaderChanges["Authorization"] != 1 {
		t.Fatalf("header change sample: samples=%d changes=%v", m.HeaderChangeSamples, m.HeaderChanges)
	}
	encoded, _ := json.Marshal(m)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "Bearer") {
		t.Fatalf("metrics expose header values: %s", encoded)
	}
}

func TestResponsesWSHandshakeTLSPhase(t *testing.T) {
	u := &wsPrewarmUpstream{}
	server := httptest.NewTLSServer(http.HandlerFunc(u.serve))
	defer server.Close()
	s := newWSTestService(t, server.URL, true)
	s.responsesWS.prewarm = false
	s.automaticGzipTransport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if w := wsTestRequest(t, s, "client", "/v1/responses", wsPrewarmBody); w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	m := s.MetricsSnapshot().ResponsesWS
	if m.HandshakeTLS.Count != 1 || m.HandshakeTCP.Count != 1 || m.HandshakeUpgrade.Count != 1 {
		t.Fatalf("handshake phases: tcp=%+v tls=%+v upgrade=%+v", m.HandshakeTCP, m.HandshakeTLS, m.HandshakeUpgrade)
	}
	var total int64
	for _, bucket := range m.HandshakeTLS.Buckets {
		total += bucket.Count
	}
	if total+m.HandshakeTLS.Overflow != 1 || m.HandshakeTLS.SumMs <= 0 {
		t.Fatalf("histogram buckets: %+v", m.HandshakeTLS)
	}
}
