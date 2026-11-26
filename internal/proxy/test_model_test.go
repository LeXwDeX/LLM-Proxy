package proxy

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
	// OpenAI 目标按其原生模式逐个测试：chat 与 responses 都发，各自报告。
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
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
	if len(paths) != 2 || paths[0] != "/v1/chat/completions" || paths[1] != "/v1/responses" {
		t.Errorf("paths = %v, want [chat, responses]", paths)
	}
	if got := modeIDs(result); got != "chat,responses" {
		t.Errorf("modes = %s, want chat,responses", got)
	}
}
func TestTestModelOpenAIReportsEachMode(t *testing.T) {
	// chat 返回 400、responses 返回 200：两个模式都测，各自报告；整体不算通过。
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"either input or prompt is required"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_123","output":[]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	result, err := s.TestModel("test-target", "gpt-5.5")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if result.Success {
		t.Fatal("a failing mode must keep the overall result from passing")
	}
	if len(paths) != 2 || paths[0] != "/v1/chat/completions" || paths[1] != "/v1/responses" {
		t.Errorf("paths = %v, want [chat, responses]", paths)
	}
	if got := modeStates(result); got != "chat:false,responses:true" {
		t.Errorf("mode states = %s, want chat:false,responses:true", got)
	}
	for _, want := range []string{"1、CHAT模式：失败", "either input or prompt is required", "2、响应模式：成功"} {
		if !strings.Contains(result.Summary, want) {
			t.Errorf("summary should contain %q, got:\n%s", want, result.Summary)
		}
	}
}
func TestTestModelAzureReportsEachMode(t *testing.T) {
	// Azure reasoning 模型（GPT-5 系列）：chat 返回 400、responses 返回 200，两个模式都测并分别报告。
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
	if got := modeStates(result); got != "chat:false,responses:true" {
		t.Errorf("mode states = %s, want chat:false,responses:true (paths %v)", got, paths)
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
	// dual_protocol 测 OpenAI Chat 与 Anthropic 两侧；OpenAI 侧使用 openai_prefix。
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
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
	if len(paths) == 0 || paths[0] != "/compatible-mode/v1/chat/completions" {
		t.Errorf("paths = %v, want first /compatible-mode/v1/chat/completions", paths)
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
			// chat 探测按用例设计返回 400，responses 返回 200：两个模式都测并各自报告。
			if err != nil || modeStates(result) != "chat:false,responses:true" {
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

// 首次探测（chat）的失败必须可见：上游不支持 Responses 时，回退探测的 404
// 不能掩盖 chat 的真实状态码与报错体（Issue：模型测试只回报最后一次回退请求）。
func TestTestModelFailureReportsEveryAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"1211","message":"model not available for this plan"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"status":404,"error":"Not Found","path":"/v1/responses"}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeOpenAI, upstream.URL)
	result, err := s.TestModel("test-target", "gpt-4o")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if result.Success {
		t.Fatal("expected failure")
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: %+v", len(result.Attempts), result.Attempts)
	}
	first := result.Attempts[0]
	if first.Path != "/v1/chat/completions" || first.StatusCode != http.StatusBadRequest {
		t.Errorf("first attempt = %+v, want chat 400", first)
	}
	if !strings.Contains(first.BodyPreview, "model not available for this plan") {
		t.Errorf("first attempt body preview lost upstream message: %q", first.BodyPreview)
	}
	if got := result.Attempts[1]; got.Path != "/v1/responses" || got.StatusCode != http.StatusNotFound {
		t.Errorf("second attempt = %+v, want responses 404", got)
	}
	for _, want := range []string{"tried 2 attempts", "HTTP 400", "model not available for this plan", "HTTP 404"} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("error should contain %q, got: %s", want, result.Error)
		}
	}
}

// dual_protocol 目标未开启“上游支持 Responses”时只测 chat 与 Anthropic 两个模式，
// 不探测 /v1/responses，与运行时 SupportsPath 的判定一致。
func TestTestModelDualProtocolWithoutResponsesTestsChatAndAnthropic(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeDualProtocol, upstream.URL, func(tg *config.Target) {
		tg.OpenAIPrefix = "/api/coding/paas/v4"
		tg.AnthropicPrefix = "/api/anthropic"
	})
	result, err := s.TestModel("test-target", "glm-5.3-flash")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	want := []string{"/api/coding/paas/v4/chat/completions", "/api/anthropic/v1/messages"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("upstream paths = %v, want %v", paths, want)
	}
	if got := modeIDs(result); got != "chat,anthropic" {
		t.Errorf("modes = %s, want chat,anthropic", got)
	}
	if !result.Success {
		t.Errorf("expected success, got: %s", result.Error)
	}
	if strings.Contains(result.Summary, "响应模式") {
		t.Errorf("summary must not mention the responses mode when it is disabled:\n%s", result.Summary)
	}
}

// 全开就全测：dual_protocol 开启 supports_responses 后依次测 chat、responses、anthropic，
// 逐个汇总成功/失败，失败项带上游错误；Anthropic 侧与真实转发一致：Bearer 认证并带 anthropic-version。
func TestTestModelDualProtocolTestsEveryEnabledMode(t *testing.T) {
	var paths []string
	var anthropicAuth, anthropicVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"timestamp":"2026-11-26T05:12:47.635+00:00","status":404,"error":"Not Found","path":"/v4/responses"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			anthropicAuth = r.Header.Get("Authorization")
			anthropicVersion = r.Header.Get("anthropic-version")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeDualProtocol, upstream.URL, func(tg *config.Target) {
		tg.SupportsResponses = true
		tg.OpenAIPrefix = "/api/coding/paas/v4"
		tg.AnthropicPrefix = "/api/anthropic"
	})
	result, err := s.TestModel("test-target", "glm-5.3-flash")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	want := []string{"/api/coding/paas/v4/chat/completions", "/api/coding/paas/v4/responses", "/api/anthropic/v1/messages"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("upstream paths = %v, want %v", paths, want)
	}
	if anthropicAuth != "Bearer sk-test-key" || anthropicVersion == "" {
		t.Errorf("anthropic mode should authenticate like real traffic, got Authorization=%q anthropic-version=%q", anthropicAuth, anthropicVersion)
	}
	if got := modeStates(result); got != "chat:true,responses:false,anthropic:true" {
		t.Fatalf("mode states = %s", got)
	}
	if result.Success {
		t.Error("one failing mode must keep the overall result from passing")
	}
	lines := strings.Split(result.Summary, "\n")
	if len(lines) != 3 {
		t.Fatalf("summary should have one line per mode, got %d:\n%s", len(lines), result.Summary)
	}
	for i, want := range []string{"1、CHAT模式：成功", "2、响应模式：失败", "3、CLAUDE模式：成功"} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("summary line %d = %q, want prefix %q", i+1, lines[i], want)
		}
	}
	if !strings.Contains(lines[1], "HTTP 404") || !strings.Contains(lines[1], "Not Found") {
		t.Errorf("failed mode must show what the endpoint answered, got %q", lines[1])
	}
}

func TestTestModelClaudeSummaryHasOneMode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeClaude, upstream.URL)
	result, err := s.TestModel("test-target", "claude-sonnet-5-5")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if !result.Success || modeIDs(result) != "anthropic" || !strings.HasPrefix(result.Summary, "1、CLAUDE模式：成功") {
		t.Errorf("success=%v modes=%s summary=%q", result.Success, modeIDs(result), result.Summary)
	}
}

func modeIDs(r *TestModelResult) string {
	ids := make([]string, 0, len(r.Modes))
	for _, m := range r.Modes {
		ids = append(ids, m.Mode)
	}
	return strings.Join(ids, ",")
}

func modeStates(r *TestModelResult) string {
	states := make([]string, 0, len(r.Modes))
	for _, m := range r.Modes {
		states = append(states, fmt.Sprintf("%s:%v", m.Mode, m.Success))
	}
	return strings.Join(states, ",")
}

// 各家上游的错误体形态不同；摘要只提取 code 与 message，解析不了就留空（回退到原文预览）。
func TestParseUpstreamErrorBody(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantCode    string
		wantMessage string
	}{
		{"openai style string code", `{"error":{"code":"1220","message":"您无权访问glm-5.4。"}}`, "1220", "您无权访问glm-5.4。"},
		{"numeric code", `{"error":{"code":429,"message":"rate limited"}}`, "429", "rate limited"},
		{"type used when code is absent", `{"error":{"type":"invalid_request_error","message":"bad model"}}`, "invalid_request_error", "bad model"},
		{"anthropic envelope", `{"type":"error","error":{"type":"permission_error","code":"1220","message":"[1220][no access]"}}`, "1220", "[1220][no access]"},
		{"spring boot 404", `{"timestamp":"2026-11-26T05:12:47.635+00:00","status":404,"error":"Not Found","path":"/v4/responses"}`, "404", "Not Found"},
		{"flat code and msg", `{"code":1000,"msg":"身份验证失败。","success":false}`, "1000", "身份验证失败。"},
		{"top level message", `{"message":"upstream exploded"}`, "", "upstream exploded"},
		{"plain text", `model not supported by any target`, "", ""},
		{"truncated json", `{"error":{"code":"1220","message":"您无权`, "", ""},
		{"empty", ``, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, message := parseUpstreamErrorBody(tc.body)
			if code != tc.wantCode || message != tc.wantMessage {
				t.Errorf("parseUpstreamErrorBody = (%q, %q), want (%q, %q)", code, message, tc.wantCode, tc.wantMessage)
			}
		})
	}
}

// 失败摘要应是“路径 → HTTP 状态 [错误码] 消息”，而不是整段原始 JSON；
// 每次探测一行，便于管理端直接阅读。
func TestTestModelFailureUsesStructuredSummary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":"1220","message":"您无权访问glm-5.4。"}}`))
	}))
	defer upstream.Close()

	s := newTestModelService(t, config.EndpointTypeDualProtocol, upstream.URL, func(tg *config.Target) {
		tg.OpenAIPrefix = "/api/coding/paas/v4"
	})
	result, err := s.TestModel("test-target", "glm-5.4")
	if err != nil {
		t.Fatalf("TestModel error: %v", err)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want chat and anthropic", result.Attempts)
	}
	got := result.Attempts[0]
	if got.ErrorCode != "1220" || got.ErrorMessage != "您无权访问glm-5.4。" {
		t.Errorf("attempt error fields = (%q, %q), want (1220, 您无权访问glm-5.4。)", got.ErrorCode, got.ErrorMessage)
	}
	if !strings.Contains(result.Error, "HTTP 403 [1220] 您无权访问glm-5.4。") {
		t.Errorf("error should carry the concise summary, got: %q", result.Error)
	}
	if strings.Contains(result.Error, `{"error"`) {
		t.Errorf("error should not repeat the raw JSON body, got: %q", result.Error)
	}
	if !strings.HasPrefix(result.Error, "upstream returned HTTP 403") || !strings.Contains(result.Error, "\n") {
		t.Errorf("error should keep its headline and put attempts on their own lines, got: %q", result.Error)
	}
}
