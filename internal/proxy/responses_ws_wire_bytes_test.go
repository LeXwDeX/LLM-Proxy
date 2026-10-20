package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
	gorillaws "github.com/gorilla/websocket"
)

// The client body keeps HTML characters, a non-sorted key order and nested
// whitespace; the proxy must forward these member bytes untouched.
const wsWireClientMembers = `"model":"test-model","input":[{"role":"user","content":"<b>a & b</b>"}],"metadata":{"z":"1","a":"2"},"store":false`

func wsWireServiceForPooled(t *testing.T, endpoint string) *Service {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 3}, Targets: []config.Target{{
		Name: "openai", EndpointType: "openai", Endpoint: endpoint, APIKey: "upstream-test-key", PreferWebSocket: true,
		ModelMappings: []config.ModelMapping{{Upstream: "test-model"}},
	}}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func wsWireSemantics(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON %q: %v", data, err)
	}
	return decoded
}

func TestResponsesWSPooledForwardsRequestBytesWithoutReencoding(t *testing.T) {
	frames := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		frames <- msg
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
	}))
	defer upstream.Close()
	s := wsWireServiceForPooled(t, upstream.URL)
	body := `{` + wsWireClientMembers + `,"stream":false,"background":false,"stream_id":"x","type":"ignored"}`
	if w := wsTestRequest(t, s, "client", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	wire := <-frames
	if bytes.Contains(wire, []byte("\\u003c")) || bytes.Contains(wire, []byte("\\u0026")) || !bytes.Contains(wire, []byte(wsWireClientMembers)) {
		t.Fatalf("client members were re-encoded: %s", wire)
	}
	want := wsWireSemantics(t, []byte(`{`+wsWireClientMembers+`}`))
	want["type"] = "response.create"
	if got := wsWireSemantics(t, wire); !reflect.DeepEqual(got, want) {
		t.Fatalf("create frame semantics changed:\n got %v\nwant %v", got, want)
	}
}

func TestResponsesWSPooledStreamsEventBytesWithoutReencoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		for _, event := range []string{
			`{"type":"response.output_text.delta","stream_id":"s1","delta":"<b>a & b</b>","z":1,"a":2}`,
			// A pretty-printed event must still become one SSE data line.
			"{\n  \"type\": \"response.output_text.delta\",\n  \"delta\": \"<i>\"\n}",
			`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		} {
			if c.Write(r.Context(), websocket.MessageText, []byte(event)) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	s := wsWireServiceForPooled(t, upstream.URL)
	w := wsTestRequest(t, s, "client", "/v1/responses", `{"model":"test-model","input":[],"stream":true}`)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	output := w.Body.String()
	if strings.Contains(output, "\\u003c") || strings.Contains(output, "stream_id") ||
		!strings.Contains(output, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<b>a & b</b>\",\"z\":1,\"a\":2}\n\n") ||
		!strings.Contains(output, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<i>\"}\n\n") {
		t.Fatalf("SSE events were re-encoded or kept stream_id:\n%s", output)
	}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if line, ok := strings.CutPrefix(scanner.Text(), "data: "); ok && !json.Valid([]byte(line)) {
			t.Fatalf("SSE data line is not one JSON event: %q", line)
		}
	}
}

func TestResponsesWebsocketNativeForwardsBytesWithoutReencoding(t *testing.T) {
	frames := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gorillaws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		frames <- msg
		_ = conn.WriteMessage(gorillaws.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"<b>a & b</b>","z":1,"a":2}`))
		_ = conn.WriteMessage(gorillaws.TextMessage, []byte("{\n \"type\": \"response.completed\",\n \"response\": {\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n}"))
	}))
	defer upstream.Close()
	service := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	principal := newAllowAllPrincipal(t, "ws-client")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(func() { server.Close(); service.Close() })
	conn := dialWebsocketTest(t, server)
	create := `{"type":"response.create",` + wsWireClientMembers + `,"stream":false,"background":false}`
	if err := conn.WriteMessage(gorillaws.TextMessage, []byte(create)); err != nil {
		t.Fatal(err)
	}
	wire := <-frames
	if bytes.Contains(wire, []byte("\\u003c")) || !bytes.Contains(wire, []byte(wsWireClientMembers)) {
		t.Fatalf("client members were re-encoded: %s", wire)
	}
	want := wsWireSemantics(t, []byte(`{`+wsWireClientMembers+`}`))
	want["type"] = "response.create"
	if got := wsWireSemantics(t, wire); !reflect.DeepEqual(got, want) {
		t.Fatalf("create frame semantics changed:\n got %v\nwant %v", got, want)
	}
	_, delta, err := conn.ReadMessage()
	if err != nil || string(delta) != `{"type":"response.output_text.delta","delta":"<b>a & b</b>","z":1,"a":2}` {
		t.Fatalf("delta was re-encoded: %s %v", delta, err)
	}
	_, completed, err := conn.ReadMessage()
	if err != nil || !json.Valid(completed) || wsWireSemantics(t, completed)["type"] != "response.completed" {
		t.Fatalf("terminal event: %s %v", completed, err)
	}
}
