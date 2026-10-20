package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

func TestResponsesWebsocketHandshakePreservesHTTPErrorBody(t *testing.T) {
	for _, test := range []struct {
		name, typ, code, reason, adapter string
		status                           int
		exhausted                        bool
	}{
		{"authentication", "authentication_error", "invalid_api_key", "invalid_token", "standard", 401, true},
		{"billing", "billing_error", "payment_required", "billing_error", "standard", 402, true},
		{"rate_limit", "rate_limit_error", "rate_limit_exceeded", "", "standard", 429, false},
		{"quota", "insufficient_quota", "organization_spend_limit_exceeded", "quota_exceeded", "standard", 429, true},
		{"server_error", "server_error", "service_unavailable", "", "standard", 503, false},
		{"kimi_quota", "exceeded_current_quota_error", "", "quota_exceeded", "kimi", 429, true},
		{"kimi_rate", "rate_limit_reached_error", "", "", "kimi", 429, false},
		{"kimi_overload", "engine_overloaded_error", "", "", "kimi", 429, false},
		{"kimi_auth", "invalid_authentication_error", "", "invalid_token", "kimi", 401, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Classification fields follow a message longer than Dial's 1 KiB
			// debug excerpt. Preserving only that excerpt cannot classify quota.
			body := fmt.Sprintf(`{"error":{"message":%q,"type":%q,"code":%q}}`, strings.Repeat("mock error detail ", 100), test.typ, test.code)
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer mock-upstream-key" || r.Header.Get("X-Mock-Setting") != "preserved" || r.Host != "mock-upstream.example" {
					t.Errorf("native handshake lost assembled request settings")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			service := &Service{httpClient: upstream.Client()}
			req, err := http.NewRequest(http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"mock-model","input":"hi"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "mock-upstream.example"
			req.Header.Set("Authorization", "Bearer mock-upstream-key")
			req.Header.Set("X-Mock-Setting", "preserved")
			target := &Target{Name: "mock-target", EndpointType: config.EndpointTypeOpenAI, UsageAdapter: test.adapter}
			session := &responsesWebsocketSession{}
			response, err := session.do(req, target, service)
			var handshake *responsesWebsocketHandshakeError
			if response != nil || !errors.As(err, &handshake) {
				t.Fatalf("HTTP rejection did not retain typed handshake details: response=%v error=%v", response, err)
			}
			if handshake.status != test.status || string(handshake.body) != body {
				t.Fatalf("handshake lost HTTP status or complete error envelope: status=%d body_bytes=%d want=%d", handshake.status, len(handshake.body), len(body))
			}
			exhausted, reason := targetKeyExhausted(target, config.ProtocolOpenAIResponses, handshake.status, handshake.body)
			if exhausted != test.exhausted || reason != test.reason {
				t.Fatalf("saved envelope cannot classify credential outcome: exhausted=%t reason=%q", exhausted, reason)
			}
			if attempts.Load() != 1 || session.httpFallback {
				t.Fatal("rejected handshake was retried or fell back to a generation")
			}
		})
	}
}

func TestResponsesWebsocketHandshakeErrorBodyReadHonorsCancellation(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: upstream.Client()})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second || attempts.Load() != 1 {
		t.Fatalf("reading a handshake error ignored request cancellation: err=%v elapsed=%s attempts=%d", err, time.Since(started), attempts.Load())
	}
}

type nativeReviewRoundTripper func(*http.Request) (*http.Response, error)

func (f nativeReviewRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type nativeReviewHandshakeBody struct {
	io.Reader
	read   atomic.Int64
	closed atomic.Int32
}

func (b *nativeReviewHandshakeBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}
func (b *nativeReviewHandshakeBody) Close() error { b.closed.Add(1); return nil }

func TestResponsesWebsocketHandshakeErrorBodyReadIsBounded(t *testing.T) {
	// The configured transport supplies the response body; the local server is
	// only a safe endpoint for the old implementation which ignored it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }))
	defer upstream.Close()
	body := &nativeReviewHandshakeBody{Reader: strings.NewReader(strings.Repeat("x", 128<<10))}
	client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: body, Request: r}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	_, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
	var handshake *responsesWebsocketHandshakeError
	if !errors.As(err, &handshake) || handshake.status != 429 {
		t.Fatalf("oversized HTTP rejection lost its status: %v", err)
	}
	if len(handshake.body) > 64<<10 || body.read.Load() > (64<<10)+1 || body.closed.Load() != 1 {
		t.Fatalf("handshake body was not bounded and released: retained=%d read=%d closes=%d", len(handshake.body), body.read.Load(), body.closed.Load())
	}
}

type nativeReviewBrokenHandshakeBody struct{}

func (nativeReviewBrokenHandshakeBody) Read(p []byte) (int, error) {
	return copy(p, `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`), io.ErrUnexpectedEOF
}

func TestResponsesWebsocketHandshakeBodyReadErrorDoesNotInventKeyFeedback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	defer upstream.Close()
	body := &nativeReviewHandshakeBody{Reader: nativeReviewBrokenHandshakeBody{}}
	client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: body, Request: r}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	_, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
	var handshake *responsesWebsocketHandshakeError
	if !errors.As(err, &handshake) || handshake.status != 401 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body read failure hid the HTTP rejection: %v", err)
	}
	if len(handshake.body) != 0 || body.closed.Load() != 1 {
		t.Fatal("incomplete error body was used for key feedback or was not closed")
	}
}

func TestResponsesWebsocketHandshakeCompleteCaptureBoundaries(t *testing.T) {
	valid := `{"error":{"code":"invalid_api_key"}}`
	for _, tc := range []struct {
		name     string
		reader   io.Reader
		complete bool
	}{
		{"empty", strings.NewReader(""), true},
		{"malformed", strings.NewReader(`{"error":`), true},
		{"exact64KiB", strings.NewReader(valid + strings.Repeat(" ", responsesWebsocketHandshakeMaxBody-len(valid))), true},
		{"over64KiB", strings.NewReader(valid + strings.Repeat(" ", responsesWebsocketHandshakeMaxBody+1-len(valid))), false},
		{"validPrefixReadError", nativeReviewBrokenHandshakeBody{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &nativeReviewHandshakeBody{Reader: tc.reader}
			client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 401, Header: http.Header{"Request-Id": {"boundary-request"}}, Body: body, Request: r}, nil
			})}
			req, _ := http.NewRequest(http.MethodPost, "http://local.invalid/v1/responses", strings.NewReader(`{"model":"mock-model"}`))
			_, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
			var handshake *responsesWebsocketHandshakeError
			if !errors.As(err, &handshake) || handshake.complete != tc.complete || body.closed.Load() != 1 || body.read.Load() > responsesWebsocketHandshakeMaxBody+1 || handshake.header.Get("Request-Id") != "boundary-request" {
				t.Fatalf("capture boundary/ownership lost: handshake=%+v reads=%d closes=%d", handshake, body.read.Load(), body.closed.Load())
			}
			if !tc.complete && len(handshake.body) != 0 {
				t.Fatal("partial rejection retained a classification envelope")
			}
		})
	}
}

func TestResponsesWebsocketHandshakeNormalizedBodyOwnership(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint(compressed), func(t *testing.T) {
			envelope := fmt.Sprintf(`{"error":{"message":%q,"type":"exceeded_current_quota_error"}}`, strings.Repeat("quota detail ", 500))
			wire := []byte(envelope)
			if compressed {
				var buffer bytes.Buffer
				zw := gzip.NewWriter(&buffer)
				_, _ = zw.Write(wire)
				_ = zw.Close()
				wire = buffer.Bytes()
			}
			original := &nativeReviewHandshakeBody{Reader: bytes.NewReader(wire)}
			service, _, recorder := websocketTestProxy(t, "http://local.invalid")
			state, _ := service.targetByName("test-target")
			state.target.UsageAdapter = "kimi"
			service.httpClient = &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": {"application/json"}, "Content-Length": {fmt.Sprint(len(wire))}, "Request-Id": {"quota-request"}}
				if compressed {
					header.Set("Content-Encoding", "gzip")
				}
				return &http.Response{StatusCode: 429, Header: header, Body: original, Request: r}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, responsesPath, nil)
			session := &responsesWebsocketSession{}
			req = req.WithContext(context.WithValue(req.Context(), responsesWebsocketContextKey{}, session))
			resp, cancel, _, err := service.forwardRequest(req, state, []byte(`{"model":"Codex-Upstream","input":"hi"}`), "", nil)
			defer deferCancel(cancel)
			if err != nil || resp == nil || responseWebsocketRejection(resp) == nil {
				t.Fatalf("completed rejection was not normalized: response=%v err=%v", resp, err)
			}
			data, readErr := io.ReadAll(resp.Body)
			if readErr != nil || string(data) != envelope || resp.Header.Get("Content-Encoding") != "" || resp.ContentLength != int64(len(envelope)) || resp.Header.Get("Content-Length") != fmt.Sprint(len(envelope)) || upstreamResponseRequestID(resp.Header) != "quota-request" {
				t.Fatalf("saved response metadata/body mismatch: header=%v bytes=%d err=%v", resp.Header, len(data), readErr)
			}
			if original.closed.Load() != 1 || state.inFlight.Load() != 1 || session.httpFallback || session.turnFailed || state.Stats().TotalSuccess != 0 {
				t.Fatal("capture prematurely released slots, settled health, or changed native turn state")
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			// The attempt finalizer is idempotent, but the shared release wrapper
			// continues to follow its ordinary single-Close ownership contract.
			_ = responseWebsocketRejection(resp).Close()
			waitWebsocketSlotsReleased(t, service)
			if original.closed.Load() != 1 || state.Stats().TotalSuccess != 1 || state.Stats().TotalFailure != 0 || state.keyPool.exhaustReasonAt(0) != "quota_exceeded_api" || service.metrics.totalFailures.Load() != 0 || len(recorder.snapshot()) != 0 {
				t.Fatalf("saved rejection close/feedback was not independent and one-shot: closes=%d stats=%+v", original.closed.Load(), state.Stats())
			}
		})
	}
}

func TestResponsesWebsocketHandshakeIncompleteCaptureDoesNotRotate(t *testing.T) {
	for _, status := range []int{401, 402, 429} {
		for _, oversized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/oversized=%v", status, oversized), func(t *testing.T) {
				service, _, recorder := websocketTestProxy(t, "http://local.invalid")
				var attempts atomic.Int32
				var originals []*nativeReviewHandshakeBody
				service.httpClient = &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
					attempts.Add(1)
					var reader io.Reader = nativeReviewBrokenHandshakeBody{}
					if oversized {
						reader = strings.NewReader(`{"error":{"code":"invalid_api_key"}}` + strings.Repeat(" ", responsesWebsocketHandshakeMaxBody))
					}
					original := &nativeReviewHandshakeBody{Reader: reader}
					originals = append(originals, original)
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: original, Request: r}, nil
				})}
				req := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(`{"model":"codex-fast","input":"hi"}`))
				req = req.WithContext(auth.WithPrincipal(context.WithValue(req.Context(), responsesWebsocketContextKey{}, &responsesWebsocketSession{}), newAllowAllPrincipal(t, "local-client")))
				writer := httptest.NewRecorder()
				service.ServeHTTP(writer, req)
				state, _ := service.targetByName("test-target")
				waitWebsocketSlotsReleased(t, service)
				if writer.Code != status || attempts.Load() != 1 || len(originals) != 1 || originals[0].closed.Load() != 1 || state.keyPool.exhaustReasonAt(0) != "" || state.keyPool.exhaustReasonAt(1) != "" || len(recorder.snapshot()) != 0 {
					t.Fatalf("incomplete rejection was classified/retried: status=%d attempts=%d reasons=%s/%s", writer.Code, attempts.Load(), state.keyPool.exhaustReasonAt(0), state.keyPool.exhaustReasonAt(1))
				}
			})
		}
	}
}

type nativeReviewBlockingHandshakeBody struct {
	done   chan struct{}
	once   sync.Once
	closed atomic.Int32
}

func (b *nativeReviewBlockingHandshakeBody) Read([]byte) (int, error) {
	<-b.done
	return 0, io.ErrUnexpectedEOF
}
func (b *nativeReviewBlockingHandshakeBody) Close() error {
	b.closed.Add(1)
	b.once.Do(func() { close(b.done) })
	return nil
}

func TestResponsesWebsocketHandshakeCustomBodyReadCancellation(t *testing.T) {
	body := &nativeReviewBlockingHandshakeBody{done: make(chan struct{})}
	client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: body, Request: r}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid/v1/responses", strings.NewReader(`{"model":"mock-model"}`))
	_, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
	if !errors.Is(err, context.DeadlineExceeded) || body.closed.Load() != 1 {
		t.Fatalf("custom body did not cancel and close once: err=%v closes=%d", err, body.closed.Load())
	}
}

func TestResponsesWebsocketHandshakeReconnectPreservesContinuationGuard(t *testing.T) {
	var badHandshakes, creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-test-key" && badHandshakes.Add(1) > 1 {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"code":"invalid_api_key"}}`)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "previous", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
	}))
	defer upstream.Close()
	service, _, recorder := websocketTestProxy(t, upstream.URL)
	state, _ := service.targetByName("test-target")
	session := &responsesWebsocketSession{}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"Codex-Upstream","input":"initial"}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp, err := session.do(req, state.target, service)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	select {
	case <-session.conn.done:
	case <-time.After(time.Second):
		t.Fatal("first connection never closed")
	}
	identity := session.identity
	turn := func(payload string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(payload))
		request = request.WithContext(auth.WithPrincipal(context.WithValue(request.Context(), responsesWebsocketContextKey{}, session), newAllowAllPrincipal(t, "local-client")))
		writer := httptest.NewRecorder()
		service.ServeHTTP(writer, request)
		return writer
	}
	if failed := turn(`{"model":"codex-fast","previous_response_id":"previous","input":"continue"}`); failed.Code != 409 || creates.Load() != 1 || badHandshakes.Load() != 1 || session.identity != identity || !session.established || session.target != "test-target" {
		t.Fatalf("continuation crossed or cleared lineage: status=%d creates=%d handshakes=%d", failed.Code, creates.Load(), badHandshakes.Load())
	}
	// Mirror the downstream per-turn reset, never the established identity.
	session.failure, session.turnStatus, session.turnFailed = "", 0, false
	if healthy := turn(`{"model":"codex-fast","input":"full input"}`); healthy.Code != 200 || !strings.Contains(healthy.Body.String(), "response.completed") {
		t.Fatalf("full-input reconnect did not rotate before create: status=%d body=%q", healthy.Code, healthy.Body.String())
	}
	waitWebsocketSlotsReleased(t, service)
	if badHandshakes.Load() != 2 || creates.Load() != 2 || state.keyPool.exhaustReasonAt(0) != "invalid_token" || len(recorder.snapshot()) != 1 {
		t.Fatalf("reconnect replayed or lost accounting: handshakes=%d creates=%d usage=%+v", badHandshakes.Load(), creates.Load(), recorder.snapshot())
	}
}

func TestResponsesWebsocketHandshakeUsesConfiguredTLSTransportAcrossTurns(t *testing.T) {
	var generations, handshakes atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if r.Host != "mock-upstream.example" || r.Header.Get("Authorization") != "Bearer mock-upstream-key" {
			t.Errorf("TLS handshake lost Host or authentication")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var payload map[string]any
			if conn.ReadJSON(&payload) != nil {
				return
			}
			turn := generations.Add(1)
			if payload["type"] != "response.create" || payload["stream"] != nil || payload["background"] != nil || (turn == 2 && payload["previous_response_id"] != "turn-1") {
				t.Errorf("native request conversion or continuation was lost")
			}
			if conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("turn-%d", turn)}}) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	client := *upstream.Client()
	base := client.Transport
	var roundTrips atomic.Int32
	client.Transport = nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		roundTrips.Add(1)
		return base.RoundTrip(r)
	})
	service := &Service{httpClient: &client}
	session := &responsesWebsocketSession{}
	defer func() {
		if session.conn != nil {
			_ = session.conn.Close()
		}
	}()
	for turn := 1; turn <= 2; turn++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		payload := map[string]any{"model": "mock-model", "stream": true, "background": false}
		if turn == 2 {
			payload["previous_response_id"] = "turn-1"
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+responsesPath, strings.NewReader(string(body)))
		req.Host = "mock-upstream.example"
		req.Header.Set("Authorization", "Bearer mock-upstream-key")
		response, err := session.do(req, &Target{Name: "mock-target", EndpointType: config.EndpointTypeOpenAI}, service)
		if err != nil {
			cancel()
			t.Fatalf("configured TLS transport could not complete turn %d: %v", turn, err)
		}
		data, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		// Ending the first generation must not cancel the reused socket.
		cancel()
		if readErr != nil || closeErr != nil || !strings.Contains(string(data), fmt.Sprintf(`"id":"turn-%d"`, turn)) {
			t.Fatalf("persistent TLS turn %d failed: read=%v close=%v data=%q", turn, readErr, closeErr, data)
		}
	}
	if roundTrips.Load() != 1 || handshakes.Load() != 1 || generations.Load() != 2 {
		t.Fatalf("TLS continuation redialed or replayed: roundtrips=%d handshakes=%d generations=%d", roundTrips.Load(), handshakes.Load(), generations.Load())
	}
}

func TestResponsesWebsocketHandshakeDoesNotFollowRedirect(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(401)
	}))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", other.URL+responsesPath)
		w.WriteHeader(http.StatusFound)
		io.WriteString(w, `{"error":{"type":"redirect_rejected"}}`)
	}))
	defer upstream.Close()
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	req.Header.Set("Authorization", "Bearer mock-upstream-key")
	_, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: upstream.Client()})
	var handshake *responsesWebsocketHandshakeError
	if !errors.As(err, &handshake) || handshake.status != 502 || redirected.Load() != 0 {
		t.Fatalf("native upgrade followed a redirect or lost rejection: err=%v redirected=%d", err, redirected.Load())
	}
}

func TestResponsesWebsocketHandshakeGzipQuotaBodyIsClassifiable(t *testing.T) {
	body := fmt.Sprintf(`{"error":{"message":%q,"type":"insufficient_quota","code":"organization_spend_limit_exceeded"}}`, strings.Repeat("mock quota detail ", 100))
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("configured HTTP transport did not negotiate automatic decompression")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusTooManyRequests)
		writer := gzip.NewWriter(w)
		_, _ = io.WriteString(writer, body)
		_ = writer.Close()
	}))
	defer upstream.Close()
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	target := &Target{EndpointType: config.EndpointTypeOpenAI}
	_, err := (&responsesWebsocketSession{}).do(req, target, &Service{httpClient: upstream.Client()})
	var handshake *responsesWebsocketHandshakeError
	if !errors.As(err, &handshake) || handshake.status != 429 || string(handshake.body) != body {
		t.Fatalf("compressed quota envelope was lost: err=%v", err)
	}
	if exhausted, reason := targetKeyExhausted(target, config.ProtocolOpenAIResponses, handshake.status, handshake.body); !exhausted || reason != "quota_exceeded" || attempts.Load() != 1 {
		t.Fatalf("compressed quota classification failed: exhausted=%t reason=%q attempts=%d", exhausted, reason, attempts.Load())
	}
}
