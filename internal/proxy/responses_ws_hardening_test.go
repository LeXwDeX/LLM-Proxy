package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	gorillaws "github.com/gorilla/websocket"
)

func TestResponsesWSCapacityFallsBackToHTTPBeforeSend(t *testing.T) {
	var gets, posts atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "unexpected handshake", 500)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"http_response","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	s.responsesWS.mu.Lock()
	for i := 0; i < responsesWSMax; i++ {
		s.responsesWS.entries[&responsesWSConn{created: time.Now(), used: time.Now(), busy: true}] = struct{}{}
	}
	s.responsesWS.mu.Unlock()
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if w.Code != 200 || gets.Load() != 0 || posts.Load() != 1 {
		t.Fatalf("full pool did not fall back to HTTP: status=%d GET=%d POST=%d body=%s", w.Code, gets.Load(), posts.Load(), w.Body.String())
	}
}

func TestResponsesWSRemembersUnsupportedUpstream(t *testing.T) {
	var gets, posts atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"http_response","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	for i := 0; i < 3; i++ {
		if w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`); w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if gets.Load() != 1 || posts.Load() != 3 {
		t.Fatalf("unsupported upstream re-probed every request: GET=%d POST=%d", gets.Load(), posts.Load())
	}
	// A configuration change forgets the result so a fixed upstream is retried.
	s.responsesWS.invalidate(false)
	wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`)
	if gets.Load() != 2 {
		t.Fatalf("invalidate kept stale unsupported result: GET=%d", gets.Load())
	}
}

func TestResponsesWebsocketNativeRemembersUnsupportedUpstream(t *testing.T) {
	var gets, posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	for i := 0; i < 2; i++ {
		conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"})
		if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
			t.Fatalf("turn %d: %v", i, event)
		}
	}
	if gets.Load() != 1 || posts.Load() != 2 {
		t.Fatalf("native fallback re-probed every turn: GET=%d POST=%d", gets.Load(), posts.Load())
	}
}

func TestResponsesWSDiscardsStrayFramesBeforeReuse(t *testing.T) {
	var upgrades atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		upgrades.Add(1)
		for n := 1; ; n++ {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"r`+string(rune('0'+n))+`","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
			// A late frame from the finished turn must not leak into the next one.
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"STALE"}`))
		}
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"one","stream":true}`); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	time.Sleep(100 * time.Millisecond)
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"two","stream":true,"previous_response_id":"r1"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "STALE") || !strings.Contains(w.Body.String(), `"r2"`) {
		t.Fatalf("stale frame crossed turns: status=%d body=%s", w.Code, w.Body.String())
	}
	if upgrades.Load() != 1 {
		t.Fatalf("lane was not reused: upgrades=%d", upgrades.Load())
	}
}

func TestResponsesWebsocketNativeDiscardsStrayFramesBetweenTurns(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for n := 1; ; n++ {
			var request map[string]any
			if conn.ReadJSON(&request) != nil {
				return
			}
			id := "resp_" + string(rune('0'+n))
			conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
			conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "STALE"})
		}
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "one"})
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("first turn: %v", event)
	}
	time.Sleep(100 * time.Millisecond)
	conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "two"})
	event := readWebsocketEvent(t, conn)
	if event["type"] != "response.completed" {
		t.Fatalf("stale frame delivered as the next turn's first event: %v", event)
	}
	if response, _ := event["response"].(map[string]any); response["id"] != "resp_2" {
		t.Fatalf("turn mixed with previous response: %v", event)
	}
}

func TestResponsesWSRetiresIdleLaneThatStopsAnsweringPings(t *testing.T) {
	setWebsocketTestDuration(t, &responsesWSReapInterval, 20*time.Millisecond)
	setWebsocketTestDuration(t, &responsesWSPingAfter, 10*time.Millisecond)
	setWebsocketTestDuration(t, &responsesWSPingTimeout, 100*time.Millisecond)
	var upgrades atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		n := upgrades.Add(1)
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`))
		if n == 1 {
			// Simulate a silently dropped path: never read again, so pings go unanswered.
			<-r.Context().Done()
			return
		}
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	if w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.responsesWS.mu.Lock()
		lanes := len(s.responsesWS.entries)
		s.responsesWS.mu.Unlock()
		if lanes == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unresponsive idle lane was not retired")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`); w.Code != 200 || upgrades.Load() != 2 {
		t.Fatalf("request after retirement: status=%d upgrades=%d", w.Code, upgrades.Load())
	}
}

func TestResponsesWSDoesNotRememberAmbiguous400(t *testing.T) {
	var gets, posts atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			http.Error(w, "bad handshake header", http.StatusBadRequest)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"http_response","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer u.Close()
	s := newWSTestService(t, u.URL, true)
	for i := 0; i < 2; i++ {
		if w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"openai-test","input":"hi"}`); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	// 400 may come from one client's own headers; it must not disable WS for others.
	if gets.Load() != 2 || posts.Load() != 2 {
		t.Fatalf("400 handshake was cached as unsupported: GET=%d POST=%d", gets.Load(), posts.Load())
	}
}

func TestResponsesWSUnsupportedMarkIgnoresStaleGeneration(t *testing.T) {
	p := newResponsesWSPool()
	defer p.close()
	identity := [32]byte{7}
	_, gen := p.knownUnsupported(identity)
	p.invalidate(false) // configuration changed while the handshake was in flight
	p.markUnsupported(identity, gen)
	if known, _ := p.knownUnsupported(identity); known {
		t.Fatal("a handshake from the old configuration repopulated the cache")
	}
}

func TestResponsesWSCanceledBeforeSendRecordsNoUsage(t *testing.T) {
	s, rec, u := newWSLifecycleHarness(t, wsModeComplete)
	principal := wsPrincipalFor(t, "client")
	if w := wsServeRequest(s, principal, nil, "/v1/responses", `{"model":"openai-test","input":"one"}`); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	// The continuation reuses the idle lane without a handshake, so a context
	// that is already canceled reaches the send step with nothing written.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wsServeRequest(s, principal, ctx, "/v1/responses", `{"model":"openai-test","input":"two","previous_response_id":"resp_1"}`)
	wsWaitForZeroInflight(t, s)
	if events := rec.snapshot(); len(events) != 1 {
		t.Fatalf("a request canceled before sending was billed as incomplete: %+v", events)
	}
	wsRequireNoProtocolViolations(t, u, 1)
}
