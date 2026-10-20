package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/coder/websocket"
)

// failingClientWriter models a downstream client that stopped reading: every
// body write fails while the upstream generation itself is healthy.
type failingClientWriter struct {
	header http.Header
	status int
}

func (w *failingClientWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *failingClientWriter) WriteHeader(status int) { w.status = status }
func (w *failingClientWriter) Write([]byte) (int, error) {
	return 0, errors.New("write: broken pipe")
}

// nativeFailingClientWriter is the native WS adapter's view of the same failure.
type nativeFailingClientWriter struct{ failingClientWriter }

func (w *nativeFailingClientWriter) downstreamWriteFailed() bool { return true }

func wsReviewRequest(t *testing.T, principal *auth.Principal, body string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r.WithContext(auth.WithPrincipal(r.Context(), principal))
}

func TestResponsesWSClientWriteFailureDoesNotMuteTarget(t *testing.T) {
	t.Run("http_client_over_upstream_pool", func(t *testing.T) {
		s, _, u := newWSLifecycleHarness(t, wsModeComplete)
		r := wsReviewRequest(t, wsPrincipalFor(t, "client"), `{"model":"openai-test","input":"hi","stream":true}`, nil)
		s.ServeHTTP(&failingClientWriter{}, r)
		wsWaitForZeroInflight(t, s)
		state, _ := s.targetByName("openai")
		if state.IsMuted(time.Now()) || state.Stats().ConsecutiveFailure != 0 {
			t.Fatalf("client write failure blamed on upstream: muted=%t failures=%d", state.IsMuted(time.Now()), state.Stats().ConsecutiveFailure)
		}
		wsRequireNoProtocolViolations(t, u, 1)
	})
	t.Run("native_websocket_client", func(t *testing.T) {
		u, endpoint := newWSLifecycleUpstreamServer(t, wsModeComplete)
		s := newWSTestService(t, endpoint, false)
		r := wsReviewRequest(t, wsPrincipalFor(t, "client"), `{"model":"openai-test","input":"hi","stream":true}`, nil)
		session := &responsesWebsocketSession{}
		r = r.WithContext(context.WithValue(r.Context(), responsesWebsocketContextKey{}, session))
		s.ServeHTTP(&nativeFailingClientWriter{}, r)
		if session.conn != nil {
			session.conn.Close()
		}
		wsWaitForZeroInflight(t, s)
		state, _ := s.targetByName("openai")
		if state.IsMuted(time.Now()) || state.Stats().ConsecutiveFailure != 0 {
			t.Fatalf("native client write failure blamed on upstream: muted=%t failures=%d", state.IsMuted(time.Now()), state.Stats().ConsecutiveFailure)
		}
		wsRequireNoProtocolViolations(t, u, 1)
	})
}

func TestResponsesWSVolatileHeadersKeepConnectionIdentity(t *testing.T) {
	s, _, u := newWSLifecycleHarness(t, wsModeComplete)
	// Exact upstream handshake counts below; background spare lanes are covered
	// by responses_ws_prewarm_test.go.
	s.responsesWS.prewarm = false
	principal := wsPrincipalFor(t, "client")
	first := httptest.NewRecorder()
	s.ServeHTTP(first, wsReviewRequest(t, principal, `{"model":"openai-test","input":"one"}`, map[string]string{
		"X-Session-ID": "codex-1", "Traceparent": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-0000000000000001-01", "X-Stainless-Retry-Count": "0",
	}))
	second := httptest.NewRecorder()
	s.ServeHTTP(second, wsReviewRequest(t, principal, `{"model":"openai-test","input":"two","previous_response_id":"resp_1"}`, map[string]string{
		"X-Session-ID": "codex-1", "Traceparent": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-0000000000000002-01", "X-Stainless-Retry-Count": "1",
	}))
	if first.Code != 200 || second.Code != 200 || u.upgrades.Load() != 1 {
		t.Fatalf("per-request trace headers split the connection: first=%d second=%d upgrades=%d body=%s", first.Code, second.Code, u.upgrades.Load(), second.Body.String())
	}
	wsRequireNoProtocolViolations(t, u, 2)
}

func TestResponsesWSFreshRequestKeepsOtherConversationContinuable(t *testing.T) {
	s, _, u := newWSLifecycleHarness(t, wsModeComplete)
	principal := wsPrincipalFor(t, "client")
	for i, body := range []string{
		`{"model":"openai-test","input":"conversation x"}`,
		`{"model":"openai-test","input":"conversation y"}`,
		`{"model":"openai-test","input":"x continues","previous_response_id":"resp_1"}`,
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, wsReviewRequest(t, principal, body, nil))
		if w.Code != 200 {
			t.Fatalf("request %d status=%d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	if got := u.snapshotPreviousIDs(); len(got) != 3 || got[2] != "resp_1" {
		t.Fatalf("continuation did not reach upstream: %v", got)
	}
	wsRequireNoProtocolViolations(t, u, 3)
}

func TestResponsesWSLocalRejectionIsNotAHealthProbe(t *testing.T) {
	s, _, u := newWSLifecycleHarness(t, wsModeComplete)
	state, _ := s.targetByName("openai")
	// One failure long ago: cooling has expired, so the next request holds the half-open probe.
	state.MarkFailure(time.Now().Add(-time.Hour), s.quietPeriod)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, wsReviewRequest(t, wsPrincipalFor(t, "client"), `{"model":"openai-test","input":"hi","previous_response_id":"resp_missing"}`, nil))
	if w.Code != 400 || u.upgrades.Load() != 0 {
		t.Fatalf("expected local previous_response_not_found: status=%d upgrades=%d", w.Code, u.upgrades.Load())
	}
	if state.Stats().ConsecutiveFailure != 1 {
		t.Fatalf("local rejection changed breaker streak to %d without contacting upstream", state.Stats().ConsecutiveFailure)
	}
	if state.probeSlotBusy(time.Now()) {
		t.Fatal("local rejection left the half-open probe lease held")
	}
}

func TestResponsesWSNonstreamUnclassifiedErrorIsNotOK(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			testResponsesWSUnclassifiedError(t, stream)
		})
	}
}

func testResponsesWSUnclassifiedError(t *testing.T, stream bool) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`))
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","error":{"type":"context_window_exceeded","message":"too long"}}`))
	}))
	defer upstream.Close()
	s := newWSTestService(t, upstream.URL, true)
	body := `{"model":"openai-test","input":"hi"}`
	if stream {
		body = `{"model":"openai-test","input":"hi","stream":true}`
	}
	w := wsTestRequest(t, s, "client", "/v1/responses", body)
	if !strings.Contains(w.Body.String(), "context_window_exceeded") || (!stream && w.Code < 400) {
		t.Fatalf("mid-generation error returned status=%d body=%s", w.Code, w.Body.String())
	}
	wsWaitForZeroInflight(t, s)
	// A request-shaped error must not mute a target shared by every client.
	if state, _ := s.targetByName("openai"); state.IsMuted(time.Now()) {
		t.Fatal("unclassified request error muted the target")
	}
}
