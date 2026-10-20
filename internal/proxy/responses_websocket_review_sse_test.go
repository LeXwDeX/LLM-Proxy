package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestResponsesWebsocketFallbackSSEFramingAndUsage(t *testing.T) {
	for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		for _, multiline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/multiline=%t", ending.name, multiline), func(t *testing.T) {
				nl := ending.value
				payload := `{"type":"response.completed","response":{"id":"sse-framing","status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`
				if multiline {
					payload = strings.Replace(payload, `,"response":`, ","+nl+"data: \"response\":", 1)
				}
				stream := ": keepalive" + nl + "event: response.completed" + nl + "id: fixture" + nl + "retry: 1000" + nl + "data: " + payload + nl + nl + "data: [DONE]" + nl + nl
				var handshakes, posts atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if websocket.IsWebSocketUpgrade(r) {
						handshakes.Add(1)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					// Split CRLF, field names, and JSON across upstream writes.
					for i := 0; i < len(stream); i++ {
						if _, err := w.Write([]byte{stream[i]}); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}))
				defer upstream.Close()
				service, server, recorder := websocketTestProxy(t, upstream.URL)
				conn := dialWebsocketTest(t, server)
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
					t.Fatal(err)
				}
				event := readWebsocketEvent(t, conn)
				if event["type"] != "response.completed" {
					t.Fatalf("valid %s SSE completion was lost: %v", ending.name, event)
				}
				waitWebsocketSlotsReleased(t, service)
				events := recorder.snapshot()
				if len(events) != 1 || events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete {
					t.Fatalf("joined SSE usage was not accounted exactly once: %+v", events)
				}
				if handshakes.Load() != 1 || posts.Load() != 1 || service.metrics.totalSuccess.Load() != 1 {
					t.Fatalf("fallback replayed or lost terminal state: handshakes=%d posts=%d successes=%d", handshakes.Load(), posts.Load(), service.metrics.totalSuccess.Load())
				}
			})
		}
	}
}

func TestResponsesWebsocketFallbackMultilineSSEFailedOutcome(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.failed\r\ndata: {\"type\":\"response.failed\",\r\ndata: \"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_input\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\r\n\r\n")
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.failed" {
		t.Fatalf("failed terminal event was lost: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	if events := recorder.snapshot(); len(events) != 1 || events[0].StatusCode != 400 || events[0].InputTokens != 10 || events[0].CostIncomplete {
		t.Fatalf("failed-response usage/status was lost: %+v", events)
	}
	if service.metrics.totalSuccess.Load() != 0 || service.metrics.totalFailures.Load() != 1 {
		t.Fatal("failed terminal event was counted as success")
	}
}

func TestResponsesWebsocketSSERejectsOversizedUnterminatedEvent(t *testing.T) {
	writer := &responsesWebsocketWriter{header: make(http.Header)}
	if _, err := writer.Write([]byte("data: " + strings.Repeat("x", responsesWebsocketMaxMessage))); err == nil {
		t.Fatal("unterminated SSE event exceeded the native message limit without an error")
	}
}

func TestResponsesWebsocketSSELeadingBOMAndPayloadWhitespace(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%t", split), func(t *testing.T) {
			payload := `  {"type":"response.completed","response":{"id":"bom"}}  `
			stream := "\xef\xbb\xbfdata: " + payload + "\r\n\r\ndata:   [DONE]  \r\n\r\n"
			result := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					result <- err
					return
				}
				defer conn.Close()
				writer := &responsesWebsocketWriter{conn: conn, header: make(http.Header)}
				if split {
					for i := 0; i < len(stream); i++ {
						if _, err = writer.Write([]byte{stream[i]}); err != nil {
							break
						}
					}
				} else {
					_, err = writer.Write([]byte(stream))
				}
				if err == nil && !writer.terminal {
					err = fmt.Errorf("BOM-prefixed terminal event was lost")
				}
				result <- err
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, frame, readErr := conn.ReadMessage()
			writeErr := <-result
			if readErr != nil || writeErr != nil || string(frame) != payload {
				t.Fatalf("BOM/DONE processing changed or lost data: read=%v write=%v frame=%q", readErr, writeErr, frame)
			}
		})
	}
}

func TestResponsesWebsocketFallbackSSEBOMUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		stream := "\xef\xbb\xbfdata: {\"type\":\"response.completed\",\"response\":{\"id\":\"bom-usage\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
		for i := 0; i < len(stream); i++ {
			if _, err := w.Write([]byte{stream[i]}); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
		t.Fatalf("BOM-prefixed completion was lost: %v", event)
	}
	waitWebsocketSlotsReleased(t, service)
	if events := recorder.snapshot(); len(events) != 1 || events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete {
		t.Fatalf("BOM-prefixed usage was not accounted exactly once: %+v", events)
	}
}
