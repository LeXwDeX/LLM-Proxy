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

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

func nativeReviewGzip(t *testing.T, data string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := io.WriteString(writer, data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func nativeReviewDialGzip(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, http.Header{"Accept-Encoding": []string{"gzip"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestResponsesWebsocketGzipFallbackEventsAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, eventType, code string
		httpStatus, outcomeStatus                int
		usage, failed                            bool
	}{
		{"completed", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"compressed\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n", "text/event-stream", "response.completed", "", 200, 200, true, false},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_input\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\r\n\r\n", "text/event-stream", "response.failed", "", 200, 400, true, true},
		{"http_error", `{"error":{"type":"permission_error","code":"policy_denied","message":"fixture denial"}}`, "application/json", "error", "policy_denied", 403, 403, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := nativeReviewGzip(t, tc.body)
			var handshakes, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					handshakes.Add(1)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				posts.Add(1)
				var request map[string]any
				if r.Method != http.MethodPost || r.Header.Get("Accept-Encoding") != "gzip" || r.Header.Get("X-Saved-Setting") != "yes" || json.NewDecoder(r.Body).Decode(&request) != nil || request["model"] != "Codex-Upstream" || request["service_tier"] != "ultrafast" || request["stream"] != true {
					t.Errorf("gzip normalization changed the assembled POST")
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", fmt.Sprint(len(encoded)))
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write(encoded)
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			conn := nativeReviewDialGzip(t, server)
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
				t.Fatal(err)
			}
			event := readWebsocketEvent(t, conn)
			if event["type"] != tc.eventType {
				t.Fatalf("compressed fallback event was lost: %v", event)
			}
			if tc.code != "" {
				detail, _ := event["error"].(map[string]any)
				if event["status"] != float64(tc.outcomeStatus) || detail["code"] != tc.code {
					t.Fatalf("compressed HTTP business error was lost: %v", event)
				}
			}
			waitWebsocketSlotsReleased(t, service)
			events := recorder.snapshot()
			if tc.usage && (len(events) != 1 || events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete || events[0].StatusCode != tc.outcomeStatus) {
				t.Fatalf("compressed terminal usage/status was not accounted once: %+v", events)
			}
			if !tc.usage && len(events) != 0 {
				t.Fatalf("HTTP error invented usage: %+v", events)
			}
			if handshakes.Load() != 1 || posts.Load() != 1 || (service.metrics.totalFailures.Load() == 1) != tc.failed || (service.metrics.totalSuccess.Load() == 1) == tc.failed {
				t.Fatalf("gzip fallback replayed or miscounted outcome: handshakes=%d posts=%d failures=%d successes=%d", handshakes.Load(), posts.Load(), service.metrics.totalFailures.Load(), service.metrics.totalSuccess.Load())
			}
		})
	}
}

type nativeReviewGzipBody struct {
	io.ReadCloser
	closed atomic.Int32
}

func (b *nativeReviewGzipBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func TestResponsesWebsocketGzipFallbackConfiguredTransportAndResponseMetadata(t *testing.T) {
	for _, encoding := range []string{"gzip", "identity"} {
		t.Run(encoding, func(t *testing.T) {
			plain := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"transport\"}}\n\n"
			encoded := nativeReviewGzip(t, plain)
			body := &nativeReviewGzipBody{ReadCloser: io.NopCloser(bytes.NewReader(encoded))}
			var calls atomic.Int32
			client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.Method == http.MethodGet {
					return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				if r.Header.Get("Accept-Encoding") != encoding || r.Header.Get("X-Mock-Setting") != "preserved" {
					t.Errorf("normalization changed original/custom request headers")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Content-Encoding": []string{"gzip"}, "Content-Length": []string{fmt.Sprint(len(encoded))}}, ContentLength: int64(len(encoded)), Body: body, Request: r}, nil
			})}
			req, _ := http.NewRequest(http.MethodPost, "https://upstream.invalid"+responsesPath, strings.NewReader(`{"model":"mock-model","stream":true}`))
			req.Header.Set("Accept-Encoding", encoding)
			req.Header.Set("X-Mock-Setting", "preserved")
			response, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" || response.ContentLength != -1 || !response.Uncompressed {
				t.Errorf("decoded fallback retained compressed metadata: headers=%v length=%d uncompressed=%t", response.Header, response.ContentLength, response.Uncompressed)
			}
			data, err := io.ReadAll(response.Body)
			if err != nil || string(data) != plain {
				t.Errorf("configured transport gzip was not decoded: err=%v data=%q", err, data)
			}
			_ = response.Body.Close()
			_ = response.Body.Close()
			if calls.Load() != 2 || body.closed.Load() != 1 {
				t.Errorf("compressed source was replayed or closed more than once: calls=%d closes=%d", calls.Load(), body.closed.Load())
			}
		})
	}
}

func TestResponsesWebsocketGzipFallbackCorruptionIsIncompleteAndNotReplayed(t *testing.T) {
	plain := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"corrupt\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	encoded := nativeReviewGzip(t, plain)
	checksum := append([]byte(nil), encoded...)
	checksum[len(checksum)-8] ^= 0xff
	for _, tc := range []struct {
		name string
		body []byte
	}{{"invalid_header", []byte("invalid gzip data")}, {"truncated", encoded[:len(encoded)-8]}, {"checksum", checksum}} {
		t.Run(tc.name, func(t *testing.T) {
			var handshakes, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					handshakes.Add(1)
					w.WriteHeader(404)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(tc.body)
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			conn := nativeReviewDialGzip(t, server)
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
				t.Fatal(err)
			}
			_ = readWebsocketEvent(t, conn)
			waitWebsocketSlotsReleased(t, service)
			events := recorder.snapshot()
			if len(events) != 1 || service.metrics.totalSuccess.Load() != 0 || service.metrics.totalFailures.Load() != 1 {
				t.Fatalf("corrupt gzip was accepted as success: events=%+v failures=%d successes=%d", events, service.metrics.totalFailures.Load(), service.metrics.totalSuccess.Load())
			}
			if tc.name == "truncated" {
				// Truncation after the official final usage is an interruption:
				// the official values are recorded complete with a marker.
				ev := events[0]
				if ev.CostIncomplete || ev.UsageStatus != usageStatusComplete || ev.InputTokens != 10 || ev.OutputTokens != 2 || !hasGoneDiagnostic(ev.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) {
					t.Fatalf("truncated stream after official usage: %+v", ev)
				}
			} else if events[0].CostIncomplete == false || hasGoneDiagnostic(events[0].UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) {
				// Bad header or checksum: an integrity failure stays incomplete.
				t.Fatalf("corrupt gzip was accepted as complete: %+v", events[0])
			}
			if handshakes.Load() != 1 || posts.Load() != 1 {
				t.Fatalf("corrupt gzip generation replayed: handshakes=%d posts=%d", handshakes.Load(), posts.Load())
			}
		})
	}
}

func TestResponsesWebsocketGzipFallbackCancellationReleasesSlots(t *testing.T) {
	canceled := make(chan struct{})
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"cancel\"}}\n\n")
		_ = writer.Flush()
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	base := service.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var source atomic.Pointer[nativeReviewGzipBody]
	service.httpClient.Transport = nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err == nil && r.Method == http.MethodPost {
			body := &nativeReviewGzipBody{ReadCloser: response.Body}
			source.Store(body)
			response.Body = body
		}
		return response, err
	})
	conn := nativeReviewDialGzip(t, server)
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.created" {
		t.Fatalf("compressed first event was lost: %v", event)
	}
	_ = conn.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel the compressed upstream")
	}
	waitWebsocketSlotsReleased(t, service)
	if events := recorder.snapshot(); len(events) != 1 || !events[0].CostIncomplete || posts.Load() != 1 || service.metrics.totalSuccess.Load() != 0 || service.metrics.totalClientCanceled.Load() != 1 {
		t.Fatalf("canceled gzip leaked, replayed, or completed usage: %+v posts=%d successes=%d", events, posts.Load(), service.metrics.totalSuccess.Load())
	}
	state, _ := service.targetByName("test-target")
	if state.IsMuted(time.Now()) {
		t.Fatal("canceling a compressed fallback muted the reachable target")
	}
	deadline := time.Now().Add(time.Second)
	for source.Load() != nil && source.Load().closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if body := source.Load(); body == nil || body.closed.Load() != 1 {
		t.Fatal("canceled compressed source was not closed exactly once")
	}
}

func TestResponsesWebsocketGzipFallbackLazyReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &nativeReviewGzipBody{ReadCloser: io.NopCloser(strings.NewReader("invalid gzip data"))}
	client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: body, Request: r}, nil
	})}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid"+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	response, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
	if err != nil {
		t.Fatalf("already-sent POST became a pre-send error: %v", err)
	}
	defer response.Body.Close()
	for i := 0; i < 2; i++ {
		if _, err := io.ReadAll(response.Body); err == nil {
			t.Error("gzip initialization error disappeared before copy/accounting")
		}
	}
}

func TestResponsesWebsocketGzipNormalizationPreservesOrdinaryHTTPPassThrough(t *testing.T) {
	plain := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	encoded := nativeReviewGzip(t, plain)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("ordinary HTTP compression negotiation changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprint(len(encoded)))
		_, _ = w.Write(encoded)
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	req, _ := http.NewRequest(http.MethodPost, server.URL+responsesPath, strings.NewReader(`{"model":"codex-fast","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || response.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(data, encoded) {
		t.Fatalf("native gzip handling changed ordinary HTTP pass-through: status=%d encoding=%q err=%v", response.StatusCode, response.Header.Get("Content-Encoding"), err)
	}
	waitWebsocketSlotsReleased(t, service)
	if events := recorder.snapshot(); len(events) != 1 || events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete {
		t.Fatalf("ordinary compressed HTTP usage changed: %+v", events)
	}
}

type nativeReviewBlockingGzipBody struct {
	entered   chan struct{}
	done      chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
	closed    atomic.Int32
}

func (b *nativeReviewBlockingGzipBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.entered) })
	<-b.done
	return 0, io.ErrClosedPipe
}

func (b *nativeReviewBlockingGzipBody) Close() error {
	b.closed.Add(1)
	b.closeOnce.Do(func() { close(b.done) })
	return nil
}

func TestResponsesWebsocketGzipFallbackCancellationClosesContextIgnoringTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &nativeReviewBlockingGzipBody{entered: make(chan struct{}), done: make(chan struct{})}
	client := &http.Client{Transport: nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: body, Request: r}, nil
	})}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid"+responsesPath, strings.NewReader(`{"model":"mock-model"}`))
	response, err := (&responsesWebsocketSession{}).do(req, &Target{EndpointType: config.EndpointTypeOpenAI}, &Service{httpClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(response.Body)
		result <- err
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("gzip reader did not enter the context-ignoring source")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceling the custom gzip source returned %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("context cancellation did not actively close the blocked gzip source")
	}
	_ = response.Body.Close()
	_ = response.Body.Close()
	if body.closed.Load() != 1 {
		t.Fatalf("custom compressed source closed %d times", body.closed.Load())
	}
}
