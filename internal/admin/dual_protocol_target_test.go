package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func assertDualTargetReadback(t *testing.T, h http.Handler, stores testStores, openai, anthropic string, responses bool) {
	t.Helper()
	stored := mustGetTarget(t, stores.targetStore, "dual-ui")
	if stored.OpenAIPrefix != openai || stored.AnthropicPrefix != anthropic || stored.SupportsResponses != responses {
		t.Fatalf("persisted fields: %+v", stored)
	}
	rec := httptest.NewRecorder()
	sessions := NewSessionManager(config.AdminSessionConfig{Secret: "dual-test-secret", CookieName: "dual-session", TTLSeconds: 3600}, nil)
	_, cookie, err := sessions.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/data/targets", nil)
	req.AddCookie(cookie)
	sessions.Middleware(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, target := range result.Targets {
		if target["name"] == "dual-ui" {
			for field, want := range map[string]any{"openai_prefix": openai, "anthropic_prefix": anthropic, "supports_responses": responses} {
				if got, ok := target[field]; !ok || got != want {
					t.Errorf("readback %s=%v present=%v, want %v", field, got, ok, want)
				}
			}
			return
		}
	}
	t.Fatal("missing target in readback")
}

// This integration regression traverses management writes, bbolt persistence,
// runtime reload and real HTTP forwarding to an isolated fake upstream.
func TestDualProtocolAdminFieldsAffectProxy(t *testing.T) {
	paths := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, t.TempDir(), []string{"fixture-key"})
	t.Cleanup(func() { _ = service.Close() })
	body := map[string]any{
		"name": "dual-ui", "endpoint_type": config.EndpointTypeDualProtocol,
		"usage_adapter": config.UsageAdapterQwen, "endpoint": upstream.URL,
		"api_key": "fixture-key", "model_mappings": []map[string]any{{"upstream": "qwen-max"}},
		"openai_prefix": "/custom-openai", "anthropic_prefix": "/custom-anthropic", "supports_responses": true,
	}
	if rec := wsCreateTarget(t, h, body); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	assertDualTargetReadback(t, h, stores, "/custom-openai", "/custom-anthropic", true)
	store := auth.NewStore()
	if err := store.LoadFromConfig([]config.Client{{Name: "fixture", AccessKey: "fixture-client", AllowedTargets: []string{"dual-ui"}}}); err != nil {
		t.Fatal(err)
	}
	principal, ok := store.Authenticate("fixture-client")
	if !ok {
		t.Fatal("fixture authentication")
	}
	request := func(path string, wantStatus int, wantPath string) {
		t.Helper()
		payload := `{"model":"qwen-max","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
		if path == "/v1/responses" {
			payload = `{"model":"qwen-max","input":"hi","max_output_tokens":1}`
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		service.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("%s status=%d want=%d body=%s", path, rec.Code, wantStatus, rec.Body.String())
		}
		if wantPath == "" {
			select {
			case got := <-paths:
				t.Fatalf("denied request reached upstream: %s", got)
			default:
			}
		} else {
			select {
			case got := <-paths:
				if got != wantPath {
					t.Fatalf("outbound path=%q want=%q", got, wantPath)
				}
			default:
				t.Fatal("request did not reach upstream")
			}
		}
	}
	request("/v1/chat/completions", http.StatusOK, "/custom-openai/v1/chat/completions")
	request("/v1/messages", http.StatusOK, "/custom-anthropic/v1/messages")
	request("/v1/responses", http.StatusOK, "/custom-openai/v1/responses")
	// Old API clients omit the new fields: existing values must survive.
	if rec := putTarget(t, h, "dual-ui", map[string]any{"endpoint_type": config.EndpointTypeDualProtocol}); rec.Code != http.StatusOK {
		t.Fatalf("omitted update: %d %s", rec.Code, rec.Body.String())
	}
	assertDualTargetReadback(t, h, stores, "/custom-openai", "/custom-anthropic", true)
	request("/v1/responses", http.StatusOK, "/custom-openai/v1/responses")
	// Explicit empty strings clear prefixes; false closes Responses admission.
	if rec := putTarget(t, h, "dual-ui", map[string]any{"endpoint_type": config.EndpointTypeDualProtocol, "openai_prefix": "", "anthropic_prefix": "", "supports_responses": false}); rec.Code != http.StatusOK {
		t.Fatalf("clear update: %d %s", rec.Code, rec.Body.String())
	}
	assertDualTargetReadback(t, h, stores, "", "", false)
	request("/v1/chat/completions", http.StatusOK, "/v1/chat/completions")
	request("/v1/messages", http.StatusOK, "/v1/messages")
	request("/v1/responses", http.StatusBadRequest, "")
}
