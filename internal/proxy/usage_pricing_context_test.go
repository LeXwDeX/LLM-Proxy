package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPricingContextProtocolEvidence(t *testing.T) {
	cases := []struct {
		name               string
		protocol           config.ProtocolType
		request, response  string
		total              int64
		valid              bool
		tier, speed, state string
	}{
		{"chat cached subset", config.ProtocolOpenAIChat, `{"service_tier":"fast"}`, `{"service_tier":"default","usage":{"prompt_tokens":272001,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":272000}}}`, 272001, true, "standard", "", "value"},
		{"responses actual ultrafast", config.ProtocolOpenAIResponses, `{}`, `{"service_tier":"ultrafast","usage":{"input_tokens":5,"output_tokens":2}}`, 5, true, "ultrafast", "", "value"},
		{"claude cache and TTL", config.ProtocolAnthropicMessages, `{"service_tier":"priority","speed":"fast"}`, `{"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":99999,"cache_creation_input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":1},"service_tier":"priority","speed":"standard"}}`, 100001, true, "priority", "standard", "value"},
		{"chat nested envelope ignored", config.ProtocolOpenAIChat, `{}`, `{"response":{"service_tier":"fast"},"usage":{"prompt_tokens":1,"completion_tokens":2}}`, 1, true, "", "", "missing"},
		{"empty tier", config.ProtocolOpenAIChat, `{}`, `{"service_tier":"","usage":{"prompt_tokens":1,"completion_tokens":2}}`, 1, true, "", "", "empty"},
		{"invalid total", config.ProtocolOpenAIChat, `{}`, `{"service_tier":"default","usage":{"prompt_tokens":null,"completion_tokens":2}}`, 0, false, "standard", "", "value"},
		{"missing tier", config.ProtocolOpenAIChat, `{"service_tier":"fast"}`, `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`, 1, true, "", "", "missing"},
		{"null tier", config.ProtocolOpenAIChat, `{}`, `{"service_tier":null,"usage":{"prompt_tokens":1,"completion_tokens":2}}`, 1, true, "", "", "null"},
		{"unknown tier", config.ProtocolOpenAIChat, `{}`, `{"service_tier":"future","usage":{"prompt_tokens":1,"completion_tokens":2}}`, 1, true, "", "", "unknown"},
		{"claude missing partition", config.ProtocolAnthropicMessages, `{}`, `{"usage":{"input_tokens":1,"output_tokens":2}}`, 1, false, "", "", "missing"},
		{"claude overflow", config.ProtocolAnthropicMessages, `{}`, `{"usage":{"input_tokens":9223372036854775807,"output_tokens":2,"cache_read_input_tokens":1,"cache_creation_input_tokens":0}}`, 0, false, "", "", "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := usageParser{protocol: tc.protocol, requestBody: []byte(tc.request)}
			got, _, found := p.extractTokens("application/json", []byte(tc.response))
			if !found {
				t.Fatal("lost usage")
			}
			c := got.pricingContext
			if c == nil {
				t.Fatal("missing pricing context")
			}
			if c.TotalInputTokens != tc.total || c.TotalInputValid != tc.valid || c.ActualServiceTier != tc.tier || c.ActualSpeed != tc.speed || c.ResponseServiceTier.State != tc.state {
				t.Fatalf("context=%+v", c)
			}
		})
	}
}

func TestPricingContextStreamingEnvelope(t *testing.T) {
	for _, tail := range []struct{ name, body, want, state string }{
		{"omitted retains early", `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`, "fast", "value"},
		{"final overrides", `{"service_tier":"default","usage":{"prompt_tokens":10,"completion_tokens":2}}`, "standard", "value"},
		{"explicit null clears", `{"service_tier":null,"usage":{"prompt_tokens":10,"completion_tokens":2}}`, "", "null"},
	} {
		t.Run(tail.name, func(t *testing.T) {
			wire := "data: {\"service_tier\":\"fast\",\"choices\":[{\"delta\":{\"content\":\"service_tier default\"}}]}\n\ndata: " + tail.body + "\n\ndata: [DONE]\n\n"
			got, _, ok := (usageParser{protocol: config.ProtocolOpenAIChat}).extractSSE([]byte(wire))
			if !ok || got.pricingContext == nil {
				t.Fatal("missing usage/context")
			}
			if got.pricingContext.ActualServiceTier != tail.want || got.pricingContext.ResponseServiceTier.State != tail.state {
				t.Fatalf("context=%+v", got.pricingContext)
			}
		})
	}
}

func TestPricingContextNativeWebsocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			t.Error(err)
			return
		}
		if request["service_tier"] != "ultrafast" {
			t.Errorf("custom body missing: %v", request)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"service_tier": "fast"}})
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "service_tier": "default", "usage": map[string]any{"input_tokens": 272001, "output_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 272000}}}})
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	service, server, recorder := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	_ = conn.WriteJSON(map[string]any{"type": "response.create", "model": "codex-fast", "input": "hi", "service_tier": "fast"})
	for {
		if readWebsocketEvent(t, conn)["type"] == "response.completed" {
			break
		}
	}
	waitWebsocketSlotsReleased(t, service)
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	c := events[0].PricingContext
	if c == nil || c.TotalInputTokens != 272001 || !c.TotalInputValid || c.ActualServiceTier != "standard" || c.RequestServiceTier.Raw != "ultrafast" {
		t.Fatalf("context=%+v", c)
	}
}

func TestPricingContextEnvelopeOnlyAndInterruptedStreams(t *testing.T) {
	cases := []struct {
		name              string
		op                config.ProtocolType
		wire, tier, speed string
		complete          bool
	}{
		{"metadata after usage", config.ProtocolOpenAIChat, "data: {\"service_tier\":\"fast\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: {\"service_tier\":\"default\"}\n\ndata: [DONE]\n\n", "standard", "", true},
		{"responses nested early tier", config.ProtocolOpenAIResponses, "data: {\"type\":\"response.created\",\"response\":{\"service_tier\":\"fast\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n", "fast", "", true},
		{"claude final speed overrides", config.ProtocolAnthropicMessages, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"speed\":\"fast\",\"service_tier\":\"priority\",\"input_tokens\":10,\"output_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"speed\":\"standard\",\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "priority", "standard", true},
		{"interrupted retains known evidence", config.ProtocolOpenAIChat, "data: {\"service_tier\":\"fast\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n", "fast", "", false},
		{"generated fields ignored", config.ProtocolOpenAIChat, "data: {\"choices\":[{\"delta\":{\"service_tier\":\"fast\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := (usageParser{protocol: tc.op}).extractSSE([]byte(tc.wire))
			if !ok || got.pricingContext == nil {
				t.Fatal("lost usage")
			}
			c := got.pricingContext
			if c.TotalInputTokens != 10 || c.ActualServiceTier != tc.tier || c.ActualSpeed != tc.speed || (got.status == usageStatusComplete) != tc.complete {
				t.Fatalf("tokens=%+v context=%+v", got, c)
			}
		})
	}
}

// Pricing evidence is the request actually sent: the assembled forward body
// after custom_body, not the client's original body. The parser receives that
// immutable forward body directly (no GetBody re-read).
func TestPricingContextActualOutboundBody(t *testing.T) {
	var sent []byte
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"service_tier":"default","usage":{"prompt_tokens":1,"completion_tokens":2}}`)), Request: r}, nil
	})
	s, principal, recorder := newPricingFutureService(t, transport, map[string]any{"service_tier": "ultrafast"})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","service_tier":"fast","messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	s.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(string(sent), `"service_tier":"ultrafast"`) {
		t.Fatalf("custom_body not applied upstream: %s", sent)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].PricingContext == nil || events[0].PricingContext.RequestServiceTier.Raw != "ultrafast" {
		t.Fatalf("events=%+v", events)
	}
}

func BenchmarkPricingContextLongInputStream(b *testing.B) {
	request := []byte(`{"speed":"fast","messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}]}`)
	wire := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":272001,\"output_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"speed\":\"standard\"}}}\n\n" + strings.Repeat("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"token\"}}\n\n", 128) + "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":128}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, _, ok := (usageParser{protocol: config.ProtocolAnthropicMessages, requestBody: request}).extractSSE(wire)
		if !ok || got.pricingContext == nil || got.pricingContext.RequestSpeed.Raw != "fast" {
			b.Fatal("lost pricing evidence")
		}
	}
}

func TestPricingContextStreamCapturesRequestOnce(t *testing.T) {
	request := []byte(`{"service_tier":"fast"}`)
	stream := newUsageStream("text/event-stream", "", usageParser{protocol: config.ProtocolOpenAIChat, requestBody: request})
	// The request has already been sent. Later ownership reuse must not change
	// evidence recorded by the response observer while output chunks arrive.
	copy(request, []byte(`{"service_tier":null  }`))
	_, _ = stream.Write([]byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2},\"service_tier\":\"default\"}\n\ndata: [DONE]\n\n"))
	stream.Close()
	if stream.tokens.pricingContext == nil || stream.tokens.pricingContext.RequestServiceTier.Raw != "fast" {
		t.Fatalf("request snapshot changed: %+v", stream.tokens.pricingContext)
	}
}
