package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/gorilla/websocket"
)

// Fail the upgraded transport, not the handshake. This exercises the real WS
// send paths without a timing-dependent TCP reset or a production test hook.
type diagnosticWSBody struct {
	io.ReadWriteCloser
	err       error
	waitFirst bool
	written   chan struct{}
	writeOnce sync.Once
	writes    atomic.Int32
	closed    atomic.Bool
}

func (b *diagnosticWSBody) Write(p []byte) (int, error) {
	b.writes.Add(1)
	if !b.waitFirst {
		return 0, b.err
	}
	n, err := b.ReadWriteCloser.Write(p)
	b.writeOnce.Do(func() { close(b.written) })
	return n, err
}

func (b *diagnosticWSBody) Read(p []byte) (int, error) {
	if b.waitFirst {
		<-b.written
		return 0, b.err
	}
	return b.ReadWriteCloser.Read(p)
}

func (b *diagnosticWSBody) Close() error {
	b.closed.Store(true)
	b.writeOnce.Do(func() { close(b.written) })
	return b.ReadWriteCloser.Close()
}

type diagnosticLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *diagnosticLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *diagnosticLogBuffer) diagnostics(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.Buffer.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "websocket generation transport failure" {
			records = append(records, record)
		}
	}
	return records
}

func TestResponsesWSGenerationFailureDiagnostics(t *testing.T) {
	for _, path := range []string{"native_send", "pooled_send", "pooled_wait_first_event"} {
		for _, cause := range []struct {
			name   string
			err    error
			status int
		}{
			{"canceled", context.Canceled, StatusClientClosedRequest},
			{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout},
			{"network_error", &net.OpError{Op: "write", Net: "tcp", Err: errors.New("private-upstream-detail")}, http.StatusBadGateway},
		} {
			t.Run(path+"/"+cause.name, func(t *testing.T) {
				var handshakes, posts, alternate atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						posts.Add(1)
						http.Error(w, "unexpected HTTP replay", 500)
						return
					}
					c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer c.Close()
					handshakes.Add(1)
					for {
						if _, _, err := c.ReadMessage(); err != nil {
							return
						}
					}
				}))
				defer upstream.Close()
				other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					alternate.Add(1)
					http.Error(w, "unexpected target replay", 500)
				}))
				defer other.Close()
				logs := &diagnosticLogBuffer{}
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{
					{Name: "test-target", EndpointType: config.EndpointTypeOpenAI, Endpoint: upstream.URL, APIKey: "fixture-provider-secret", APIKeys: []string{"fixture-other-secret"}, MaxConcurrency: 1, PreferWebSocket: true, ModelMappings: []config.ModelMapping{{Upstream: "UpstreamCase", Fallback: "ws-model"}}},
					{Name: "alternate", EndpointType: config.EndpointTypeOpenAI, Endpoint: other.URL, APIKey: "fixture-alternate-secret", ModelMappings: []config.ModelMapping{{Upstream: "OtherCase", Fallback: "ws-model"}}},
				}}
				s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				var injected atomic.Pointer[diagnosticWSBody]
				transport := http.DefaultTransport.(*http.Transport).Clone()
				defer transport.CloseIdleConnections()
				s.httpClient.Transport = statsRoundTrip(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if err == nil && resp.StatusCode == http.StatusSwitchingProtocols {
						body := &diagnosticWSBody{ReadWriteCloser: resp.Body.(io.ReadWriteCloser), err: cause.err, waitFirst: path == "pooled_wait_first_event", written: make(chan struct{})}
						injected.Store(body)
						resp.Body = body
					}
					return resp, err
				})
				usage := &websocketUsageRecorder{}
				stats := &completionRecorder{}
				s.SetUsageRecorder(usage)
				s.SetRequestStatsRecorder(stats)
				principal := newAllowAllPrincipal(t, "diagnostic-client")
				for _, ip := range []string{"127.0.0.1", "192.0.2.1"} {
					s.affinity.Set(affinityKey(ip, principal.Name, "ws-model"), "test-target", time.Now())
				}
				proposed := cause.status
				if path == "native_send" {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						s.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
					}))
					defer server.Close()
					conn := dialWebsocketTest(t, server)
					if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "ws-model", "input": "private-request-body"}); err != nil {
						t.Fatal(err)
					}
					event := readWebsocketEvent(t, conn)
					if event["type"] != "error" || event["status"] != float64(http.StatusBadGateway) {
						t.Fatalf("native WS error event contract changed: %v", event)
					}
					proposed = http.StatusBadGateway
				} else {
					r := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(`{"model":"ws-model","input":"private-request-body","stream":true}`))
					r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if w.Code != proposed {
						t.Fatalf("pooled HTTP status=%d want %d", w.Code, proposed)
					}
				}
				deadline := time.Now().Add(2 * time.Second)
				for len(stats.outcomes()) == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				outcome := requeststats.Failure
				if path != "native_send" && cause.status == StatusClientClosedRequest {
					outcome = requeststats.Canceled
				}
				assertCompletions(t, stats, outcome)
				waitWebsocketSlotsReleased(t, s)
				events := usage.snapshot()
				if len(events) != 1 || events[0].StatusCode != cause.status || !events[0].CostIncomplete {
					t.Fatalf("usage must settle once with internal status %d: %+v", cause.status, events)
				}
				if handshakes.Load() != 1 || posts.Load() != 0 || alternate.Load() != 0 || s.metrics.totalKeyRetries.Load() != 0 || s.metrics.totalTargetRetries.Load() != 0 {
					t.Fatalf("generation replayed: upgrades=%d posts=%d alternate=%d keyRetries=%d targetRetries=%d", handshakes.Load(), posts.Load(), alternate.Load(), s.metrics.totalKeyRetries.Load(), s.metrics.totalTargetRetries.Load())
				}
				wantFailures, wantCanceled := int64(1), int64(0)
				if outcome == requeststats.Canceled {
					wantFailures, wantCanceled = 0, 1 // existing 499 metric contract
				}
				if s.metrics.totalRequests.Load() != 1 || s.metrics.totalFailures.Load() != wantFailures || s.metrics.totalClientCanceled.Load() != wantCanceled || s.metrics.totalSuccess.Load() != 0 {
					t.Fatalf("request metrics settled incorrectly: requests=%d failures=%d canceled=%d success=%d", s.metrics.totalRequests.Load(), s.metrics.totalFailures.Load(), s.metrics.totalClientCanceled.Load(), s.metrics.totalSuccess.Load())
				}
				body := injected.Load()
				if body == nil || !body.closed.Load() || body.writes.Load() != 1 {
					t.Fatalf("upgraded transport not released after one write: %+v", body)
				}
				s.responsesWS.mu.Lock()
				lanes := len(s.responsesWS.entries)
				s.responsesWS.mu.Unlock()
				if lanes != 0 {
					t.Fatalf("pooled connection leaked: lanes=%d", lanes)
				}
				records := logs.diagnostics(t)
				if len(records) != 1 {
					t.Fatalf("want one generation failure diagnostic, got %v", records)
				}
				stage := "send"
				if path == "pooled_wait_first_event" {
					stage = "wait_first_event"
				}
				classification := cause.name
				if cause.status == http.StatusBadGateway {
					classification = "other_error"
				}
				channel := "http_response"
				if path == "native_send" {
					channel = "websocket_error_event"
				}
				want := map[string]any{"failure_stage": stage, "internal_cause": classification, "internal_status": float64(cause.status), "client_proposed_status": float64(proposed), "client_status_channel": channel, "possibly_sent": true, "replayed": false, "target": "test-target", "client": principal.Name, "model": "UpstreamCase"}
				for name, value := range want {
					if records[0][name] != value {
						t.Errorf("diagnostic %s=%v want %v", name, records[0][name], value)
					}
				}
				// Native turns generate a request ID even without HTTP middleware.
				if path == "native_send" && (events[0].RequestID == "" || records[0]["request_id"] != events[0].RequestID) {
					t.Errorf("diagnostic request ID does not correlate with usage: %v", records[0])
				}
				wire, _ := json.Marshal(records[0])
				for _, secret := range []string{"fixture-provider-secret", "fixture-other-secret", "private-request-body", "private-upstream-detail"} {
					if strings.Contains(string(wire), secret) {
						t.Errorf("diagnostic leaked sensitive data: %s", secret)
					}
				}
			})
		}
	}
}
