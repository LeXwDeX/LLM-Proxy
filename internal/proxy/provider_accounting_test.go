package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

type adapterEventRecorder struct{ events []usage.Event }

func (r *adapterEventRecorder) Record(e usage.Event) error {
	r.events = append(r.events, e)
	return nil
}

func TestQwenAccountingUsesOutboundRequest(t *testing.T) {
	wire := `{"model":"qwen3.8-max","usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(wire))
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "upstream", Endpoint: upstream.URL, EndpointType: config.EndpointTypeOpenAI, APIKey: "key"}}}
	cfg.Targets[0].EndpointType = config.EndpointTypeDualProtocol
	cfg.Targets[0].UsageAdapter = config.UsageAdapterQwen
	cfg.Targets[0].ModelMappings = []config.ModelMapping{{Upstream: "qwen3.8-max", Fallback: "alias"}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	recorder := &adapterEventRecorder{}
	svc.SetUsageRecorder(recorder)
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"system","content":[{"type":"text","text":"prefix","cache_control":{"type":"ephemeral"}}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != wire {
		t.Fatalf("wire changed: %d %s", rr.Code, rr.Body.String())
	}
	if len(recorder.events) != 1 {
		t.Fatalf("events=%+v", recorder.events)
	}
	e := recorder.events[0]
	if e.ResponseModel != "qwen3.8-max" || e.UsageProtocol != string(config.ProtocolOpenAIChat) || e.Model != "qwen3.8-max" || e.InputTokens != 20 || e.CachedTokens != 30 || e.CacheCreationTokens != 50 || e.ExplicitCacheReadTokens != 30 || e.UsageAdapter != "qwen" {
		t.Fatalf("bad accounting: %+v", e)
	}
}

func TestMissingUsageRecordedAsIncomplete(t *testing.T) {
	wire := `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(wire))
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "upstream", Endpoint: upstream.URL, EndpointType: config.EndpointTypeOpenAI, APIKey: "key"}}}
	cfg.Targets[0].EndpointType = config.EndpointTypeOpenAI
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	recorder := &adapterEventRecorder{}
	svc.SetUsageRecorder(recorder)
	store := auth.NewStore()
	store.LoadFromConfig(testAuthClients("tester", "token"))
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != wire {
		t.Fatalf("wire changed: %d %s", rr.Code, rr.Body.String())
	}
	if len(recorder.events) != 1 || recorder.events[0].UsageStatus != "incomplete" || !recorder.events[0].CostIncomplete {
		t.Fatalf("missing usage hidden: %+v", recorder.events)
	}
}

func TestUsageContextUsesFinalOutboundHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("x-dashscope-session-cache", "enable")
	outbound := r.Clone(r.Context())
	outbound.Header = r.Header.Clone()
	outbound.Header.Set("x-dashscope-session-cache", "disable")
	p := newTargetUsageParser(r, &Target{UsageAdapter: "qwen"}, "qwen3.8-max", []byte(`{"input":"hi"}`), outbound)
	if p.requestHeaders.Get("x-dashscope-session-cache") != "disable" {
		t.Fatal("original header overrode actual outbound header")
	}
}

func TestAccountingCustomBodyCannotOverrideMappedModel(t *testing.T) {
	wire := `{"model":"qwen3.8-flash","usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(wire))
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "upstream", Endpoint: upstream.URL, EndpointType: config.EndpointTypeDualProtocol, APIKey: "key", UsageAdapter: "qwen", CustomBody: map[string]any{"model": "qwen3.8-flash"}}}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	recorder := &adapterEventRecorder{}
	svc.SetUsageRecorder(recorder)
	store := auth.NewStore()
	store.LoadFromConfig(testAuthClients("tester", "token"))
	principal, _ := store.Authenticate("token")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if len(recorder.events) != 1 || recorder.events[0].Model != "qwen3.8-max" {
		t.Fatalf("wrong billed model: %+v", recorder.events)
	}
}
