package proxy

import (
	"bytes"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func nativeReviewDeflate(t *testing.T, plain string) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := zlib.NewWriter(&encoded)
	if _, err := io.WriteString(writer, plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestResponsesWebsocketFallbackNegotiatesConsumableEncoding(t *testing.T) {
	const plain = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"negotiated\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	deflate, gzip := nativeReviewDeflate(t, plain), nativeReviewGzip(t, plain)
	for _, tc := range []struct {
		name, upstreamEncoding string
		clientEncoding         []string
	}{
		{"deflate_only", "identity", []string{"deflate"}},
		{"unsupported_browser_encodings", "identity", []string{"br, deflate, zstd"}},
		{"mixed", "gzip", []string{"gzip, deflate, br"}},
		{"supported_quality_preserved", "gzip;q=0.5, identity;q=0.1", []string{"deflate;q=1, gzip;q=0.5, identity;q=0.1"}},
		{"multiple_header_values", "gzip", []string{"deflate", "gzip"}},
		{"identity_preserved", "identity", []string{"identity"}},
		{"gzip_preserved", "gzip", []string{"gzip"}},
		{"transport_default", "gzip", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handshakes, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					handshakes.Add(1)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				posts.Add(1)
				accept := strings.Join(r.Header.Values("Accept-Encoding"), ", ")
				if accept != tc.upstreamEncoding {
					t.Errorf("fallback advertised an unsupported encoding or lost supported preferences: %q, want %q", accept, tc.upstreamEncoding)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				// This upstream prefers deflate when the request advertises it.
				// Both compressed responses are valid HTTP content codings.
				switch {
				case strings.Contains(accept, "deflate"):
					w.Header().Set("Content-Encoding", "deflate")
					_, _ = w.Write(deflate)
				case strings.Contains(accept, "gzip"):
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write(gzip)
				default:
					_, _ = io.WriteString(w, plain)
				}
			}))
			defer upstream.Close()
			service, server, recorder := websocketTestProxy(t, upstream.URL)
			headers := make(http.Header)
			if tc.clientEncoding != nil {
				headers["Accept-Encoding"] = tc.clientEncoding
			}
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+responsesPath, headers)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi"}); err != nil {
				t.Fatal(err)
			}
			event := readWebsocketEvent(t, conn)
			waitWebsocketSlotsReleased(t, service)
			events := recorder.snapshot()
			if event["type"] != "response.completed" || len(events) != 1 || events[0].InputTokens != 10 || events[0].OutputTokens != 2 || events[0].CostIncomplete || events[0].StatusCode != 200 {
				t.Errorf("negotiated fallback lost its terminal or complete usage: event=%v usage=%+v", event, events)
			}
			if handshakes.Load() != 1 || posts.Load() != 1 || service.metrics.totalSuccess.Load() != 1 || service.metrics.totalFailures.Load() != 0 {
				t.Errorf("negotiated fallback replayed or miscounted outcome: handshakes=%d posts=%d success=%d failure=%d", handshakes.Load(), posts.Load(), service.metrics.totalSuccess.Load(), service.metrics.totalFailures.Load())
			}
		})
	}
}

func TestResponsesWebsocketEncodingNegotiationPreservesOrdinaryHTTP(t *testing.T) {
	encoded := nativeReviewDeflate(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http\"}}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "deflate" {
			t.Errorf("ordinary HTTP encoding negotiation changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "deflate")
		_, _ = w.Write(encoded)
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	req, _ := http.NewRequest(http.MethodPost, server.URL+responsesPath, strings.NewReader(`{"model":"codex-fast","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "deflate")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || response.Header.Get("Content-Encoding") != "deflate" || !bytes.Equal(data, encoded) {
		t.Fatalf("native negotiation changed ordinary HTTP pass-through: status=%d encoding=%q err=%v", response.StatusCode, response.Header.Get("Content-Encoding"), err)
	}
}
