package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type responsesWSHandshakeStatusBody struct {
	io.ReadCloser
	closed atomic.Int32
}

func (b *responsesWSHandshakeStatusBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func TestResponsesWSFailedUpgradeCannotReturnSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusFound, http.StatusNotModified, http.StatusSwitchingProtocols} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := `{"upstream":"landing_page","padding":"` + strings.Repeat("x", 12000) + `"}`
			if status == http.StatusNoContent || status == http.StatusNotModified {
				body = ""
			} else if status == http.StatusSwitchingProtocols {
				body = "invalid upgrade"
			}
			var gets, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				gets.Add(1)
				if status == http.StatusSwitchingProtocols {
					conn, writer, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_, _ = fmt.Fprintf(writer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: invalid\r\n\r\n%s", body)
					_ = writer.Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			service := newWSTestService(t, upstream.URL, true)
			recorder := &websocketUsageRecorder{}
			service.SetUsageRecorder(recorder)
			state, _ := service.targetByName("openai")
			state.keyPool = newKeyPool("openai", []string{"upstream-test-key"}, "", newTestLogger())
			state.MarkFailure(time.Now().Add(-time.Hour), time.Nanosecond)
			base := service.httpClient.Transport
			var source atomic.Pointer[responsesWSHandshakeStatusBody]
			service.httpClient.Transport = nativeReviewRoundTripper(func(r *http.Request) (*http.Response, error) {
				response, err := base.RoundTrip(r)
				if err == nil {
					counted := &responsesWSHandshakeStatusBody{ReadCloser: response.Body}
					source.Store(counted)
					response.Body = counted
				}
				return response, err
			})
			response := wsTestRequest(t, service, "client", responsesPath, `{"model":"openai-test","input":"hello"}`)
			if response.Code != http.StatusBadGateway {
				t.Errorf("failed HTTP %d upgrade returned generation status %d", status, response.Code)
			}
			if response.Body.String() != body {
				t.Errorf("failed upgrade body was lost: bytes=%d want=%d", response.Body.Len(), len(body))
			}
			if gets.Load() != 1 || posts.Load() != 0 || service.metrics.totalSuccess.Load() != 0 || service.metrics.totalFailures.Load() != 1 {
				t.Errorf("upgrade failure replayed or counted as success: GET=%d POST=%d success=%d failure=%d", gets.Load(), posts.Load(), service.metrics.totalSuccess.Load(), service.metrics.totalFailures.Load())
			}
			if events := recorder.snapshot(); len(events) != 0 {
				t.Errorf("a rejected upgrade with no generation produced usage: %+v", events)
			}
			stats := state.Stats()
			if !state.IsMuted(time.Now()) || state.probeSlotBusy(time.Now()) || stats.TotalFailure != 2 || stats.ConsecutiveFailure != 2 {
				t.Errorf("failed upgrade did not finish the health probe: %+v probe_busy=%t", stats, state.probeSlotBusy(time.Now()))
			}
			if state.inFlight.Load() != 0 {
				t.Errorf("failed upgrade leaked target capacity: %d", state.inFlight.Load())
			}
			state.keyPool.mu.Lock()
			for index, count := range state.keyPool.inFlight {
				if count != 0 {
					t.Errorf("failed upgrade leaked key %d capacity: %d", index, count)
				}
			}
			state.keyPool.mu.Unlock()
			service.responsesWS.mu.Lock()
			entries := len(service.responsesWS.entries)
			service.responsesWS.mu.Unlock()
			if entries != 0 {
				t.Errorf("failed upgrade retained %d pool entries", entries)
			}
			if counted := source.Load(); counted == nil || counted.closed.Load() != 1 {
				t.Error("failed upgrade source body was not closed exactly once")
			}
		})
	}
}

func TestResponsesWSUnsupportedUpgradeStillFallsBack(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired, http.StatusNotImplemented} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var gets, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
					w.WriteHeader(status)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"fallback","model":"UpstreamCase","usage":{"input_tokens":2,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			service := newWSTestService(t, upstream.URL, true)
			recorder := &websocketUsageRecorder{}
			service.SetUsageRecorder(recorder)
			response := wsTestRequest(t, service, "client", responsesPath, `{"model":"openai-test","input":"hello"}`)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"fallback"`) || gets.Load() != 1 || posts.Load() != 1 {
				t.Errorf("HTTP %d unsupported upgrade lost its fallback: response=%d GET=%d POST=%d body=%s", status, response.Code, gets.Load(), posts.Load(), response.Body.String())
			}
			if events := recorder.snapshot(); len(events) != 1 || events[0].InputTokens != 2 || events[0].OutputTokens != 1 || events[0].CostIncomplete {
				t.Errorf("fallback usage was not recorded once: %+v", events)
			}
			if service.metrics.totalSuccess.Load() != 1 || service.metrics.totalFailures.Load() != 0 || service.TargetStatuses(time.Now())[0].InFlight != 0 {
				t.Error("successful fallback lost its outcome or leaked capacity")
			}
			service.responsesWS.mu.Lock()
			entries := len(service.responsesWS.entries)
			service.responsesWS.mu.Unlock()
			if entries != 0 {
				t.Errorf("unsupported upgrade retained %d pool entries", entries)
			}
		})
	}
}
