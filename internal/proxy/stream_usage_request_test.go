package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestStreamUsageRequestAssembly(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, want string
		enabled                bool
		custom                 map[string]any
	}{
		{"missing", "/v1/chat/completions", `{"stream":true}`, `{"stream":true,"stream_options":{"include_usage":true}}`, true, nil},
		{"nested", "/v1/chat/completions", `{"stream":true,"stream_options":{"other":{"x":1}}}`, `{"stream":true,"stream_options":{"other":{"x":1},"include_usage":true}}`, true, nil},
		{"large-number", "/v1/chat/completions", `{"stream":true,"stream_options":{"other":9007199254740993},"seed":9007199254740993}`, `{"stream":true,"stream_options":{"other":9007199254740993,"include_usage":true},"seed":9007199254740993}`, true, nil},
		{"azure-path", "/openai/deployments/qwen-max/chat/completions", `{"stream":true}`, `{"stream":true,"stream_options":{"include_usage":true}}`, true, nil},
		{"true", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":true}}`, "", true, nil},
		{"false", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":false}}`, "", true, nil},
		{"null-field", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":null}}`, "", true, nil},
		{"wrong-field", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":"yes"}}`, "", true, nil},
		{"number-field", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":1}}`, "", true, nil},
		{"object-field", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":{}}}`, "", true, nil},
		{"array-field", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":[]}}`, "", true, nil},
		{"null-options", "/v1/chat/completions", `{"stream":true,"stream_options":null}`, "", true, nil},
		{"array-options", "/v1/chat/completions", `{"stream":true,"stream_options":[]}`, "", true, nil},
		{"string-options", "/v1/chat/completions", `{"stream":true,"stream_options":"yes"}`, "", true, nil},
		{"number-options", "/v1/chat/completions", `{"stream":true,"stream_options":1}`, "", true, nil},
		{"default", "/v1/chat/completions", `{"stream":true}`, "", false, nil},
		{"non-stream", "/v1/chat/completions", `{"stream":false}`, "", true, nil},
		{"missing-stream", "/v1/chat/completions", `{}`, "", true, nil},
		{"wrong-stream", "/v1/chat/completions", `{"stream":"true"}`, "", true, nil},
		{"null-stream", "/v1/chat/completions", `{"stream":null}`, "", true, nil},
		{"number-stream", "/v1/chat/completions", `{"stream":1}`, "", true, nil},
		{"embeddings", "/v1/embeddings", `{"stream":true}`, "", true, nil},
		{"responses", "/v1/responses", `{"stream":true}`, "", true, nil},
		{"anthropic", "/v1/messages", `{"stream":true}`, "", true, nil},
		{"gemini", "/v1beta/models/qwen-max:streamGenerateContent", `{"stream":true}`, "", true, nil},
		{"image", "/v1/images/generations", `{"stream":true}`, "", true, nil},
		{"custom-false", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":true,"client":1}}`, `{"stream":true,"stream_options":{"include_usage":false}}`, true, map[string]any{"stream_options": map[string]any{"include_usage": false}}},
		{"custom-missing", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":false}}`, `{"stream":true,"stream_options":{"operator":1,"include_usage":true}}`, true, map[string]any{"stream_options": map[string]any{"operator": 1}}},
		{"custom-null", "/v1/chat/completions", `{"stream":true}`, `{"stream":true,"stream_options":null}`, true, map[string]any{"stream_options": nil}},
		{"custom-true", "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":false}}`, `{"stream":true,"stream_options":{"include_usage":true,"operator":1}}`, true, map[string]any{"stream_options": map[string]any{"include_usage": true, "operator": 1}}},
		{"custom-malformed", "/v1/chat/completions", `{"stream":true}`, `{"stream":true,"stream_options":"operator"}`, true, map[string]any{"stream_options": "operator"}},
		{"custom-blocked-stream", "/v1/chat/completions", `{"stream":true}`, `{"stream":true,"stream_options":{"include_usage":true}}`, true, map[string]any{"stream": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &Target{RequestStreamUsageIfMissing: tc.enabled, CustomBody: tc.custom}
			r := httptest.NewRequest("POST", tc.path, nil)
			r.Header.Set("Content-Type", "application/json")
			got := prepareTargetBody(target, r, []byte(tc.body), "qwen-max", "qwen-max")
			want := tc.want
			if want == "" {
				want = tc.body
				if string(got) != want {
					t.Fatalf("unchanged body: %s", got)
				}
			}
			var g, w any
			decodeGot := json.NewDecoder(strings.NewReader(string(got)))
			decodeGot.UseNumber()
			if err := decodeGot.Decode(&g); err != nil {
				t.Fatal(err)
			}
			decodeWant := json.NewDecoder(strings.NewReader(want))
			decodeWant.UseNumber()
			if err := decodeWant.Decode(&w); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("got %s want %s", got, want)
			}
		})
	}
}

func TestStreamUsageMalformedBodyPassthrough(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "not-json", `{"stream":true,`} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		if got := prepareTargetBody(&Target{RequestStreamUsageIfMissing: true}, r, []byte(body), "", ""); string(got) != body {
			t.Fatalf("changed %q into %s", body, got)
		}
	}
}

type streamUsageFailoverTransport struct {
	bodies []map[string]any
	hosts  []string
}

func (tr *streamUsageFailoverTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	tr.bodies = append(tr.bodies, body)
	tr.hosts = append(tr.hosts, r.URL.Host)
	if r.URL.Host == "a.invalid" {
		return nil, errors.New("connection refused")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
}
func TestStreamUsageFailoverDoesNotPolluteOriginal(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{
		{Name: "a", EndpointType: "openai", Endpoint: "http://a.invalid", APIKey: "fixture-a", RequestStreamUsageIfMissing: true, ModelMappings: []config.ModelMapping{{Upstream: "A-MODEL", Fallback: "qwen-max"}}},
		{Name: "b", EndpointType: "openai", Endpoint: "http://b.invalid", APIKey: "fixture-b", ModelMappings: []config.ModelMapping{{Upstream: "B-MODEL", Fallback: "qwen-max"}}},
	}}
	s, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr := &streamUsageFailoverTransport{}
	s.httpClient = &http.Client{Transport: tr}
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"qwen-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || len(tr.bodies) != 2 {
		t.Fatalf("status=%d hosts=%v body=%s", w.Code, tr.hosts, w.Body.String())
	}
	if tr.hosts[0] != "a.invalid" || tr.hosts[1] != "b.invalid" {
		t.Fatalf("hosts=%v", tr.hosts)
	}
	options, ok := tr.bodies[0]["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatal("A missing opt-in")
	}
	if _, present := tr.bodies[1]["stream_options"]; present {
		t.Fatalf("B inherited options: %v", tr.bodies[1])
	}
	if tr.bodies[0]["model"] != "A-MODEL" || tr.bodies[1]["model"] != "B-MODEL" {
		t.Fatalf("models=%v", tr.bodies)
	}
}
