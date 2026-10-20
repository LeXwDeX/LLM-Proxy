package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/gorilla/websocket"
)

// Three ways an upstream may read one top-level member. A request is only
// safe when all three agree with the value the proxy authorized and billed.

// oracleMapLast: encoding/json map decoding (last exact key wins).
func oracleMapLast(body []byte, key string) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "", false
	}
	raw, ok := m[key]
	return string(raw), ok
}

// oracleFirst: a first-match reader such as gjson (first exact key wins).
func oracleFirst(body []byte, key string) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", false
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return "", false
		}
		if name, _ := token.(string); name == key {
			return string(raw), true
		}
	}
	return "", false
}

// oracleStructFold: a Go struct field tagged with the key. encoding/json
// matches keys case-insensitively (Unicode simple folding) and the last
// matching member wins.
func oracleStructFold(body []byte, key string) (string, bool) {
	typ := reflect.StructOf([]reflect.StructField{{Name: "F", Type: reflect.TypeOf(json.RawMessage{}), Tag: reflect.StructTag(`json:"` + key + `"`)}})
	value := reflect.New(typ)
	if json.Unmarshal(body, value.Interface()) != nil {
		return "", false
	}
	raw := value.Elem().Field(0).Bytes()
	return string(raw), raw != nil
}

type memberReading struct {
	value   string
	present bool
}

// assertSingleReading fails unless all three readers see the same value of
// key; it returns that reading.
func assertSingleReading(t *testing.T, body []byte, key string) memberReading {
	t.Helper()
	mapValue, mapOK := oracleMapLast(body, key)
	firstValue, firstOK := oracleFirst(body, key)
	foldValue, foldOK := oracleStructFold(body, key)
	if mapOK != firstOK || mapOK != foldOK || mapValue != firstValue || mapValue != foldValue {
		t.Fatalf("member %q is ambiguous upstream: map-last=%q(%v) first=%q(%v) struct-fold=%q(%v)\nbody: %s",
			key, mapValue, mapOK, firstValue, firstOK, foldValue, foldOK, body)
	}
	return memberReading{mapValue, mapOK}
}

func assertProtectedMembersUnambiguous(t *testing.T, body []byte) {
	t.Helper()
	for _, key := range protectedRequestMembers {
		assertSingleReading(t, body, key)
	}
}

// --- HTTP probes ---

type capturedUpstream struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

func (c *capturedUpstream) lastHeader(t *testing.T) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		t.Fatal("upstream received no request")
	}
	return c.headers[len(c.headers)-1]
}

func (c *capturedUpstream) last(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no request")
	}
	return c.bodies[len(c.bodies)-1]
}

func newNormalizeHTTPService(t *testing.T, mappings []config.ModelMapping) (*Service, *capturedUpstream, *auth.Principal) {
	t.Helper()
	captured := &capturedUpstream{}
	s, err := NewService(&config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "t", EndpointType: config.EndpointTypeOpenAI, Endpoint: "http://upstream.invalid", APIKey: "fixture", UsageAdapter: "standard", ModelMappings: mappings}},
	}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.bodies = append(captured.bodies, body)
		captured.headers = append(captured.headers, r.Header.Clone())
		captured.mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"service_tier":"default","usage":{"prompt_tokens":1,"completion_tokens":1}}`)), Request: r}, nil
	})}
	recorder := &goneEventRecorder{}
	s.SetUsageRecorder(recorder)
	return s, captured, newAllowAllPrincipal(t, "fixture-normalize")
}

func serveNormalize(s *Service, principal *auth.Principal, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

var routingBypassProbes = []struct {
	name, body string
	model      string // the model every reader must see upstream ("" = absent)
	tier       string // raw service_tier every reader must see ("" = absent)
}{
	{"duplicate model, secret first", `{"model":"gpt-secret","model":"test-model","messages":[]}`, `"test-model"`, ""},
	{"capitalized variant last", `{"model":"test-model","Model":"gpt-secret","messages":[]}`, `"test-model"`, ""},
	{"upper variant first", `{"MODEL":"gpt-secret","model":"test-model","messages":[]}`, `"test-model"`, ""},
	{"escaped variant", "{\"model\":\"test-model\",\"\\u004dodel\":\"gpt-secret\",\"messages\":[]}", `"test-model"`, ""},
	{"duplicate service_tier", `{"model":"test-model","service_tier":"flex","service_tier":"priority","messages":[]}`, `"test-model"`, `"priority"`},
	{"long-s service_tier variant", `{"model":"test-model","service_tier":"flex","ſervice_tier":"priority","messages":[]}`, `"test-model"`, `"flex"`},
	{"variant only service_tier", `{"model":"test-model","Service_Tier":"priority","messages":[]}`, `"test-model"`, ""},
}

// The proxy authorizes and bills the last exact member. Upstream must read
// exactly that value whatever its parser, on the unrewritten path.
func TestRoutingMembersUnambiguousUpstreamHTTP(t *testing.T) {
	for _, probe := range routingBypassProbes {
		t.Run(probe.name, func(t *testing.T) {
			s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "test-model"}})
			if w := serveNormalize(s, principal, "/v1/chat/completions", probe.body); w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			sent := captured.last(t)
			assertProtectedMembersUnambiguous(t, sent)
			if got := assertSingleReading(t, sent, "model"); got.value != probe.model {
				t.Fatalf("upstream model = %q, want %q", got.value, probe.model)
			}
			if got := assertSingleReading(t, sent, "service_tier"); got.value != probe.tier {
				t.Fatalf("upstream service_tier = %q, want %q", got.value, probe.tier)
			}
		})
	}
}

// Same on the rewritten (alias) path and with operator custom_body.
func TestRoutingMembersUnambiguousUpstreamHTTPRewrite(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, []config.ModelMapping{{Upstream: "TEST-MODEL", Fallback: "alias"}})
	for _, body := range []string{
		`{"model":"alias","Model":"gpt-secret","messages":[]}`,
		`{"model":"gpt-secret","model":"alias","messages":[]}`,
		`{"Model":"gpt-secret","model":"alias","stream":true,"Stream":false,"messages":[]}`,
	} {
		if w := serveNormalize(s, principal, "/v1/chat/completions", body); w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		sent := captured.last(t)
		assertProtectedMembersUnambiguous(t, sent)
		if got := assertSingleReading(t, sent, "model"); got.value != `"TEST-MODEL"` {
			t.Fatalf("body %s: upstream model %q", body, got.value)
		}
	}
}

// A model the target does not serve cannot ride along as a case variant:
// the proxy saw no model, so upstream must not see one either.
func TestVariantOnlyModelNeverReachesUpstream(t *testing.T) {
	s, captured, principal := newNormalizeHTTPService(t, nil)
	// A chat request without an exact model is refused before routing.
	if w := serveNormalize(s, principal, "/v1/chat/completions", `{"Model":"gpt-secret","messages":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("variant-only chat model: status %d", w.Code)
	}
	// Paths without a required model still forward; the variant must not.
	w := serveNormalize(s, principal, "/v1/responses/resp_1/cancel", `{"Model":"gpt-secret","Service_Tier":"priority"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	sent := captured.last(t)
	for _, key := range []string{"model", "service_tier"} {
		if got := assertSingleReading(t, sent, key); got.present {
			t.Fatalf("upstream read %s=%s the proxy never authorized: %s", key, got.value, sent)
		}
	}
}

// --- native WebSocket probes ---

func TestRoutingMembersUnambiguousUpstreamNativeWebsocket(t *testing.T) {
	frames := make(chan []byte, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- data
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
		}
	}))
	defer upstream.Close()
	_, server, _ := websocketTestProxy(t, upstream.URL)
	conn := dialWebsocketTest(t, server)
	for _, payload := range []string{
		`{"type":"response.create","model":"gpt-secret","model":"codex-fast","input":"hi"}`,
		`{"type":"response.create","model":"codex-fast","Model":"gpt-secret","input":"hi"}`,
		`{"type":"response.create","Type":"response.cancel","model":"codex-fast","Service_tier":"flex","service_tier":"flex","input":"hi"}`,
		`{"type":"response.create","model":"codex-fast","Background":true,"STREAM_ID":"s","input":"hi","Input":"other"}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		if event := readWebsocketEvent(t, conn); event["type"] != "response.completed" {
			t.Fatalf("payload %s: event=%v", payload, event)
		}
		var sent []byte
		select {
		case sent = <-frames:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream received no frame")
		}
		assertProtectedMembersUnambiguous(t, sent)
		if got := assertSingleReading(t, sent, "model"); got.value != `"Codex-Upstream"` {
			t.Fatalf("payload %s: upstream model %q in %s", payload, got.value, sent)
		}
		// custom_body's service_tier is the only tier the proxy priced.
		if got := assertSingleReading(t, sent, "service_tier"); got.value != `"ultrafast"` {
			t.Fatalf("payload %s: upstream tier %q", payload, got.value)
		}
		if got := assertSingleReading(t, sent, "type"); got.value != `"response.create"` {
			t.Fatalf("payload %s: upstream type %q", payload, got.value)
		}
	}
}
