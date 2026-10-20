package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A TLS close_notify to a peer that stopped reading can block CloseNow for up
// to five seconds. Simulate that stall and require pool operations to keep
// serving other requests while lanes are being closed.
func TestResponsesWSPoolClosesConnectionsOutsideLock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
			if c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	s := newWSTestService(t, upstream.URL, true)
	s.responsesWS.prewarm = false
	for _, client := range []string{"client-a", "client-b", "client-c"} {
		if w := wsTestRequest(t, s, client, "/v1/responses", `{"model":"openai-test","input":"hi"}`); w.Code != 200 {
			t.Fatalf("%s: %d %s", client, w.Code, w.Body.String())
		}
	}
	const stall = 1500 * time.Millisecond
	release := make(chan struct{})
	stalled := func(c *websocket.Conn) {
		select {
		case <-release:
		case <-time.After(stall):
		}
		_ = c.CloseNow()
	}
	responsesWSCloseHook.Store(&stalled)
	t.Cleanup(func() {
		responsesWSCloseHook.Store(nil)
		close(release)
	})
	// A configuration change closes every idle lane.
	go s.responsesWS.invalidate(false)
	time.Sleep(50 * time.Millisecond)
	for _, client := range []string{"client-a", "client-d"} {
		started := time.Now()
		w := wsTestRequest(t, s, client, "/v1/responses", `{"model":"openai-test","input":"hi"}`)
		if elapsed := time.Since(started); w.Code != 200 || elapsed > stall/3 {
			t.Fatalf("%s acquire waited for lane closes: status=%d elapsed=%v", client, w.Code, elapsed)
		}
	}
	// The reaper closes on its own goroutine, never while holding the pool.
	go s.responsesWS.reapAt(time.Now().Add(time.Hour))
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	s.responsesWS.mu.Lock()
	lanes := len(s.responsesWS.entries)
	s.responsesWS.mu.Unlock()
	if elapsed := time.Since(started); elapsed > stall/3 || lanes != 0 {
		t.Fatalf("reap held the pool: elapsed=%v lanes=%d", elapsed, lanes)
	}
	// A request releasing an unusable lane closes it itself, but other pool
	// users are not blocked meanwhile.
	e, resp, fallback, err := s.responsesWS.acquire(t.Context(), s.httpClient, wsLockTestRequest(t, upstream.URL), [32]byte{9}, "")
	if err != nil || resp != nil || fallback || e == nil {
		t.Fatalf("acquire: conn=%v resp=%v fallback=%v err=%v", e, resp, fallback, err)
	}
	go s.responsesWS.release(e, false)
	time.Sleep(50 * time.Millisecond)
	started = time.Now()
	s.responsesWS.mu.Lock()
	s.responsesWS.mu.Unlock()
	if elapsed := time.Since(started); elapsed > stall/3 {
		t.Fatalf("release held the pool during its close: %v", elapsed)
	}
}

func wsLockTestRequest(t *testing.T, endpoint string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
