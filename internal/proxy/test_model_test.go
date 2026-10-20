package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func newTestModelService(t *testing.T, epType, upstreamURL string, extra ...func(*config.Target)) *Service {
	t.Helper()
	target := config.Target{
		Name:          "test-target",
		EndpointType:  epType,
		Endpoint:      upstreamURL,
		APIKey:        "sk-test-key",
		ModelMappings: []config.ModelMapping{{Upstream: "test-model"}},
	}
	for _, fn := range extra {
		fn(&target)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 60,
		},
		Targets: []config.Target{target},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func TestTestModelOpenAIChatSuccess(t *testing.T) {
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	result, err := s.TestModel("test-target", "gpt-4o")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	if seenPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", seenPath)
	}
}

func TestTestModelOpenAIFallbackToResponses(t *testing.T) {
	// chat/completions 返回 400，responses 返回 200 → 应自动回退
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"either input or prompt is required"}}`))
			return
		}
		// responses
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","output":[]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	result, err := s.TestModel("test-target", "gpt-5.5")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success via responses fallback, got: %s (status %d)", result.Error, result.StatusCode)
	}
	// 确认两个路径都被调用过
	if len(paths) < 2 || paths[0] != "/v1/chat/completions" || paths[1] != "/v1/responses" {
		t.Errorf("paths = %v, want [chat, responses]", paths)
	}
}

func TestTestModelAzureFallbackToResponses(t *testing.T) {
	// Azure reasoning 模型（GPT-5 系列）：chat/completions 返回 400，responses 返回 200 → 应自动回退
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("api-key") != "sk-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"model only supports responses api"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","output":[]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeAzureOpenAI, upstream.URL)
	result, err := s.TestModel("test-target", "gpt-5.6-sol")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success via responses fallback, got: %s (status %d)", result.Error, result.StatusCode)
	}
	if len(paths) < 2 {
		t.Errorf("expected chat + responses attempts, got paths: %v", paths)
	}
}

func TestTestModelClaude(t *testing.T) {
	var seenPath, seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"text":"ok"}]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeClaude, upstream.URL)
	result, err := s.TestModel("test-target", "claude-sonnet-4")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	if seenPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", seenPath)
	}
	if seenAuth != "sk-test-key" {
		t.Errorf("x-api-key = %q, want sk-test-key", seenAuth)
	}
}

func TestTestModelGeminiModelInURL(t *testing.T) {
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeGemini, upstream.URL)
	result, err := s.TestModel("test-target", "gemini-flash")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	if !strings.Contains(seenPath, "/models/gemini-flash:generateContent") {
		t.Errorf("path = %q, want contains gemini-flash:generateContent", seenPath)
	}
}

func TestTestModelImage(t *testing.T) {
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[{"url":"https://example.com/img.png"}]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAIImage, upstream.URL+"/v1/images/generations")
	result, err := s.TestModel("test-target", "gpt-image-2")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	// terminal URL 覆盖路径，实际发的路径就是 endpoint 配的路径
	if !strings.Contains(seenPath, "images/generations") {
		t.Errorf("path = %q, want contains images/generations", seenPath)
	}
}

func TestTestModelUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chat 和 responses 都返回 401
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	result, err := s.TestModel("test-target", "gpt-4o")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for 401, got success")
	}
	// openai 双探：chat 和 responses 都失败，error 应体现多次尝试
	if !strings.Contains(result.Error, "tried 2 attempts") {
		t.Errorf("error should mention 2 attempts, got: %s", result.Error)
	}
}

func TestTestModelDualProtocolUsesOpenAISide(t *testing.T) {
	// dual_protocol 应走 OpenAI 双探（chat 成功即可，不走 responses）
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeDualProtocol, upstream.URL, func(t *config.Target) {
		t.OpenAIPrefix = "/compatible-mode"
	})
	result, err := s.TestModel("test-target", "qwen-plus")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	if seenPath != "/compatible-mode/v1/chat/completions" {
		t.Errorf("path = %q, want /compatible-mode/v1/chat/completions", seenPath)
	}
}

func TestTestModelTargetNotFound(t *testing.T) {
	s := newTestModelService(t, config.EndpointTypeOpenAI, "https://example.com")
	_, err := s.TestModel("nonexistent", "gpt-4o")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestTestModelEmptyModel(t *testing.T) {
	s := newTestModelService(t, config.EndpointTypeOpenAI, "https://example.com")
	_, err := s.TestModel("test-target", "")
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("expected empty model error, got: %v", err)
	}
}

// TestTestModelMultiKeySkipsRateLimited 验证方案 A 核心需求：
// 多 key target，第一个 key（primary api_key）被上游限流（429），测试应自动顺延到第二个 key 并成功。
func TestTestModelMultiKeySkipsRateLimited(t *testing.T) {
	var seenKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenKeys = append(seenKeys, auth)
		if auth == "Bearer key-limited" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		// 其他 key: 成功
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	// 多 key target：primary api_key 限流，secondary 健康。
	// newTargetState 在 len(APIKeys) > 1 时创建 keyPool，所以至少 2 个 key。
	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL, func(tg *config.Target) {
		tg.APIKey = "key-limited"            // 合并后排在第一位
		tg.APIKeys = []string{"key-healthy"} // 合并后第二位
	})
	result, err := s.TestModel("test-target", "gpt-4o")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success via key[1], got: %s", result.Error)
	}
	// 应该先试 key-limited（429），再试 key-healthy（200）
	if len(seenKeys) < 2 {
		t.Fatalf("expected at least 2 key attempts, got %d: %v", len(seenKeys), seenKeys)
	}
	if seenKeys[0] != "Bearer key-limited" {
		t.Errorf("first attempt should use key-limited, got %s", seenKeys[0])
	}
}

// TestTestModelMultiKeyAllFail 验证：所有 key 都失败时，返回失败 + 合并错误信息。
func TestTestModelMultiKeyAllFail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL, func(tg *config.Target) {
		tg.APIKey = "key-a"            // primary
		tg.APIKeys = []string{"key-b"} // secondary → 合并后 2 个 key
	})
	result, err := s.TestModel("test-target", "gpt-4o")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when all keys fail, got success")
	}
	// openai 双探 × 2 keys = 4 attempts
	if !strings.Contains(result.Error, "4 attempts") || !strings.Contains(result.Error, "2 keys") {
		t.Errorf("error should mention attempts across keys, got: %s", result.Error)
	}
}

// Saved target settings must reach both probes, including Azure fields outside
// its normal request whitelist. The alias and protected credentials stay intact.
func TestTestModelSavedRequestSettings(t *testing.T) {
	for _, endpointType := range []string{config.EndpointTypeOpenAI, config.EndpointTypeAzureOpenAI} {
		t.Run(endpointType, func(t *testing.T) {
			var paths []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["service_tier"] != "ultrafast" {
					t.Errorf("service_tier = %v", payload["service_tier"])
				}
				if payload["model"] != "GPT-CodexUltrafast" {
					t.Errorf("model = %v", payload["model"])
				}
				if r.Header.Get("X-Test-Setting") != "saved" {
					t.Errorf("custom header = %q", r.Header.Get("X-Test-Setting"))
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("content type overridden")
				}
				if r.Header.Get(headerProxyTarget) != "" {
					t.Errorf("routing header injected")
				}
				if endpointType == config.EndpointTypeAzureOpenAI {
					if r.Header.Get("api-key") != "sk-test-key" || r.Header.Get("Authorization") != "" {
						t.Errorf("Azure credentials overridden")
					}
				} else if r.Header.Get("Authorization") != "Bearer sk-test-key" || r.Header.Get("api-key") != "" {
					t.Errorf("OpenAI credentials overridden")
				}
				if strings.HasSuffix(r.URL.Path, "/chat/completions") {
					messages, ok := payload["messages"].([]any)
					if !ok || len(messages) != 1 {
						t.Errorf("messages overridden: %v", payload["messages"])
					}
					w.WriteHeader(http.StatusBadRequest)
				} else {
					if payload["input"] != "hi" {
						t.Errorf("input overridden: %v", payload["input"])
					}
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer upstream.Close()
			s := newTestModelService(t, endpointType, upstream.URL, func(target *config.Target) {
				target.ModelMappings = []config.ModelMapping{{Upstream: "GPT-CodexUltrafast", Fallback: "codex-fast"}}
				target.CustomBody = map[string]any{"service_tier": "ultrafast"}
				target.CustomHeaders = map[string]string{"X-Test-Setting": "saved"}
			})
			// Bypass admin validation to exercise runtime protection of persisted config.
			state, _ := s.targetByName("test-target")
			target := state.Target()
			target.CustomBody["model"] = "evil"
			target.CustomBody["messages"] = []any{}
			target.CustomBody["input"] = "evil"
			target.CustomHeaders["Authorization"] = "evil"
			target.CustomHeaders["api-key"] = "evil"
			target.CustomHeaders["Content-Type"] = "text/plain"
			target.CustomHeaders["X-Proxy-Target"] = "evil"
			result, err := s.TestModel("test-target", "CODEX-FAST")
			if err != nil || !result.Success {
				t.Fatalf("test result = %+v, error = %v", result, err)
			}
			if len(paths) != 2 {
				t.Fatalf("paths = %v, want chat and responses", paths)
			}
			// The same saved settings and protection apply to ordinary requests.
			request := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(string(responsesBody("CODEX-FAST"))))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "client-secret")
			request.Header.Set("api-key", "client-secret")
			request = request.WithContext(auth.WithPrincipal(request.Context(), newAllowAllPrincipal(t, "fixture")))
			response := httptest.NewRecorder()
			s.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("ordinary request status = %d: %s", response.Code, response.Body.String())
			}
			if len(paths) != 3 {
				t.Fatalf("ordinary request did not reach upstream: %v", paths)
			}
		})
	}
}

func TestTestModelGeminiMappedPathAndSavedSettings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/Gemini-Upstream:generateContent" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "sk-test-key" {
			t.Errorf("Gemini auth missing")
		}
		if r.Header.Get("X-Test-Setting") != "saved" {
			t.Errorf("saved header missing")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["generationConfig"] == nil || payload["contents"] == nil {
			t.Errorf("payload = %v", payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	service := newTestModelService(t, config.EndpointTypeGemini, upstream.URL, func(target *config.Target) {
		target.ModelMappings = []config.ModelMapping{{Upstream: "Gemini-Upstream", Fallback: "gemini-test"}}
		target.CustomBody = map[string]any{"generationConfig": map[string]any{"temperature": 0}}
		target.CustomHeaders = map[string]string{"X-Test-Setting": "saved"}
	})
	result, err := service.TestModel("test-target", "GEMINI-TEST")
	if err != nil || !result.Success {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}
