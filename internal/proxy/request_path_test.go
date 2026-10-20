package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestUnknownPathsCannotReachUpstreamOrRecordSuccessfulUsage(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-test","usage":{"input_tokens":100,"output_tokens":10}}`))
	}))
	defer upstream.Close()
	svc, principal, recorder := newPathTestService(t, upstream.URL)

	tests := []struct {
		path       string
		body       string
		status     int
		code       string
		wantDetail string
	}{
		{"/responses", `{"model":"gpt-test","input":"hi"}`, http.StatusBadRequest, "invalid_responses_path", "/v1/responses"},
		{"/v2/responses", `{"model":"gpt-test","input":"hi"}`, http.StatusBadRequest, "invalid_responses_path", "/v1/responses"},
		{"/abc/1123", `{"model":"gpt-test","input":"hi"}`, http.StatusNotFound, "unknown_request_path", "/abc/1123"},
		{"/111/11/11", `{}`, http.StatusNotFound, "unknown_request_path", "/111/11/11"},
		{"/v1/chat/completions/extra", `{"model":"gpt-test","messages":[]}`, http.StatusNotFound, "unknown_request_path", "/v1/chat/completions/extra"},
		{"/chat/completions/extra", `{"model":"gpt-test","messages":[]}`, http.StatusNotFound, "unknown_request_path", "/chat/completions/extra"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			response := httptest.NewRecorder()
			svc.ServeHTTP(response, req)
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body requestError
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tc.code || body.Error.Path != tc.path || !strings.Contains(body.Error.Message, tc.wantDetail) {
				t.Fatalf("error=%+v", body.Error)
			}
		})
	}
	if upstreamCalls.Load() != 0 || len(recorder.events) != 0 {
		t.Fatalf("invalid paths reached upstream or recorded usage: calls=%d events=%+v", upstreamCalls.Load(), recorder.events)
	}
}

func TestClientPathClassificationKeepsSupportedProviderRoutes(t *testing.T) {
	for _, path := range []string{
		"/v1/chat/completions", "/chat/completions", "/openai/v1/chat/completions",
		"/v1/responses", "/v1/responses/resp_123", "/v1/messages/count_tokens",
		"/v1/embeddings", "/v1/images/edits", "/v1/files/file_123",
		"/openai/deployments/gpt-test/chat/completions", "/openai/deployments/gpt-test/responses",
		"/v1beta/models/gemini-test:generateContent", "/v1/models/gemini-test:countTokens",
		"/v1/models", "/models",
	} {
		if got := classifyClientPath(path); got == clientPathUnknown {
			t.Errorf("supported path %q classified as unknown", path)
		}
	}
	for _, path := range []string{"/responses", "/v2/responses", "/v1/unknown", "/abc/1123", "/111/11/11", "/v1/chat/completions/extra", "/chat/completions/extra"} {
		if got := classifyClientPath(path); got != clientPathUnknown {
			t.Errorf("unsupported path %q classified as %v", path, got)
		}
	}
}

func TestResponsesPathSeparatesMissingModelFromUnknownModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("invalid model reached upstream")
	}))
	defer upstream.Close()
	svc, principal, recorder := newPathTestService(t, upstream.URL)
	for _, tc := range []struct {
		name, body, detail string
	}{
		{"missing", `{"input":"hi"}`, "model is required"},
		{"unsupported", `{"model":"other","input":"hi"}`, "model not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			response := httptest.NewRecorder()
			svc.ServeHTTP(response, req)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.detail) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if len(recorder.events) != 0 {
		t.Fatalf("rejected models recorded usage: %+v", recorder.events)
	}
}

func TestModelRequestRequiresModelWithoutMappings(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{
		Name: "unrestricted", Endpoint: upstream.URL, EndpointType: config.EndpointTypeOpenAI, APIKey: "key",
	}}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		response := httptest.NewRecorder()
		svc.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"model_required"`) {
			t.Fatalf("path=%q status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("model-less requests reached upstream %d times", upstreamCalls.Load())
	}
}

func TestAzureV1ResponsesPathUsesResponsesFields(t *testing.T) {
	path := "/openai/v1/responses"
	if classifyClientPath(path) != clientPathResponses || ResolveProtocol(path) != config.ProtocolOpenAIResponses {
		t.Fatalf("Azure v1 Responses path was not classified as Responses")
	}
	parser := usageParser{protocol: ResolveProtocol(path)}
	tokens, _, found := parser.extractTokens("application/json", []byte(`{"usage":{"input_tokens":100,"output_tokens":10}}`))
	if !found || tokens.InputTokens != 100 || tokens.OutputTokens != 10 || tokens.status != usageStatusComplete {
		t.Fatalf("Azure v1 Responses usage=%+v found=%t", tokens, found)
	}
}

func TestCanonicalResponsesPathRecordsBillableUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		wire := `{"model":"gpt-test","usage":{"input_tokens":100,"output_tokens":10}}`
		contentType := "application/json"
		if stream {
			name = "sse"
			wire = "event: response.completed\n" +
				`data: {"type":"response.completed","response":{"model":"gpt-test","status":"completed","usage":{"input_tokens":100,"output_tokens":10}}}` + "\n\n"
			contentType = "text/event-stream"
		}
		t.Run(name, func(t *testing.T) {
			var upstreamPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamPath = r.URL.Path
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte(wire))
			}))
			defer upstream.Close()
			svc, principal, recorder := newPathTestService(t, upstream.URL)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			response := httptest.NewRecorder()
			svc.ServeHTTP(response, req)
			if response.Code != http.StatusOK || upstreamPath != "/v1/responses" {
				t.Fatalf("status=%d upstream_path=%q body=%s", response.Code, upstreamPath, response.Body.String())
			}
			if len(recorder.events) != 1 {
				t.Fatalf("events=%+v", recorder.events)
			}
			event := recorder.events[0]
			if event.UsageProtocol != string(config.ProtocolOpenAIResponses) || event.UsageStatus != usage.UsageStatusComplete || event.CostIncomplete || event.InputTokens != 100 || event.OutputTokens != 10 {
				t.Fatalf("wrong Responses usage: %+v", event)
			}
			rates := usage.CostTable{"gpt-test": {InputPer1MTokens: 2, OutputPer1MTokens: 10}}
			if cost := usage.EstimateEventCost(event, rates); math.Abs(cost-0.0003) > 1e-12 {
				t.Fatalf("cost=%g, want 0.0003", cost)
			}
		})
	}
}

func TestChatPathsRemainBillableForChatOnlyModels(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					t.Errorf("upstream path=%q, want %q", r.URL.Path, path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"gpt-test","usage":{"prompt_tokens":100,"completion_tokens":10}}`))
			}))
			defer upstream.Close()
			svc, principal, recorder := newPathTestService(t, upstream.URL)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
			response := httptest.NewRecorder()
			svc.ServeHTTP(response, req)
			if response.Code != http.StatusOK || len(recorder.events) != 1 {
				t.Fatalf("status=%d events=%+v body=%s", response.Code, recorder.events, response.Body.String())
			}
			event := recorder.events[0]
			if event.UsageProtocol != string(config.ProtocolOpenAIChat) || event.InputTokens != 100 || event.OutputTokens != 10 || event.UsageStatus != usage.UsageStatusComplete {
				t.Fatalf("wrong Chat usage: %+v", event)
			}
			rates := usage.CostTable{"gpt-test": {InputPer1MTokens: 2, OutputPer1MTokens: 10}}
			if cost := usage.EstimateEventCost(event, rates); math.Abs(cost-0.0003) > 1e-12 {
				t.Fatalf("cost=%g, want 0.0003", cost)
			}
		})
	}
}

func TestAzureDeploymentResponsesUsesResponsesAccounting(t *testing.T) {
	const path = "/openai/deployments/gpt-test/responses"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("upstream path=%q, want %q", r.URL.Path, path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-test","usage":{"input_tokens":100,"output_tokens":10}}`))
	}))
	defer upstream.Close()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{
		Name: "upstream", Endpoint: upstream.URL, EndpointType: config.EndpointTypeAzureOpenAI,
		APIKey: "key", ModelMappings: []config.ModelMapping{{Upstream: "gpt-test"}},
	}}}
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
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	response := httptest.NewRecorder()
	svc.ServeHTTP(response, req)
	if response.Code != http.StatusOK || len(recorder.events) != 1 {
		t.Fatalf("status=%d events=%+v body=%s", response.Code, recorder.events, response.Body.String())
	}
	event := recorder.events[0]
	if event.UsageProtocol != string(config.ProtocolOpenAIResponses) || event.InputTokens != 100 || event.OutputTokens != 10 || event.UsageStatus != usage.UsageStatusComplete {
		t.Fatalf("wrong Azure Responses usage: %+v", event)
	}
}

func newPathTestService(t *testing.T, endpoint string) (*Service, *auth.Principal, *adapterEventRecorder) {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{
		Name: "upstream", Endpoint: endpoint, EndpointType: config.EndpointTypeOpenAI,
		APIKey: "key", SupportsResponses: true,
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-test"}},
	}}}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	recorder := &adapterEventRecorder{}
	svc.SetUsageRecorder(recorder)
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("principal missing")
	}
	return svc, principal, recorder
}
