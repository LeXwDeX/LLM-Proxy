package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Every supported family traverses the real forwarding/recording path. Tool
// signatures and vendor extensions remain opaque to accounting on both wires.
func TestProviderProtocolAccountingMatrix(t *testing.T) {
	cases := []struct {
		name, endpointType, adapter, model, path, usage, wantAdapter string
		in, out, cache, write                                        int64
	}{
		{"openai-chat", "openai", "standard", "o3", "/v1/chat/completions", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":4}}`, usageAdapterStandardOpenAI, 70, 10, 30, 0},
		{"openai-responses", "openai", "standard", "o3", "/v1/responses", `{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":30}}`, usageAdapterStandardOpenAI, 70, 10, 30, 0},
		{"claude", "claude", "standard", "claude-sonnet-5", "/v1/messages", `{"input_tokens":20,"output_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":30}}`, usageAdapterStandardAnthropic, 20, 10, 30, 50},
		{"gemini", "gemini", "standard", "gemini-2.5-pro", "/v1beta/models/gemini-2.5-pro:generateContent", `{"promptTokenCount":100,"candidatesTokenCount":6,"thoughtsTokenCount":4,"cachedContentTokenCount":30}`, usageAdapterStandardGemini, 70, 10, 30, 0},
		{"gemini-vendor-native-usage", "gemini", "qwen", "unlisted-model", "/v1beta/models/unlisted-model:generateContent", `{"promptTokenCount":100,"candidatesTokenCount":6,"thoughtsTokenCount":4,"cachedContentTokenCount":30}`, usageAdapterStandardGemini, 70, 10, 30, 0},
		{"qwen-chat", "dual_protocol", "qwen", "qwen3.8-max", "/v1/chat/completions", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_input_tokens":50}}`, usageAdapterQwen, 20, 10, 30, 50},
		{"qwen-responses", "dual_protocol", "qwen", "qwen3.8-flash", "/v1/responses", `{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":30},"x_details":[{"input_tokens_details":{"cache_creation_input_tokens":50}}]}`, usageAdapterQwen, 20, 10, 30, 50},
		{"qwen-messages", "dual_protocol", "qwen", "qwen3.8-max", "/v1/messages", `{"input_tokens":20,"output_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":50}`, usageAdapterQwen, 20, 10, 30, 50},
		{"glm-chat", "dual_protocol", "glm", "glm-5", "/v1/chat/completions", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":30}}`, usageAdapterGLM, 70, 10, 30, 0},
		{"glm-messages", "dual_protocol", "glm", "glm-5", "/v1/messages", `{"input_tokens":20,"output_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":50}`, usageAdapterGLM, 20, 10, 30, 50},
		{"deepseek-chat", "dual_protocol", "deepseek", "deepseek-chat", "/v1/chat/completions", `{"prompt_tokens":100,"completion_tokens":10,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}`, usageAdapterDeepSeek, 70, 10, 30, 0},
		{"deepseek-responses", "dual_protocol", "deepseek", "deepseek-chat", "/v1/responses", `{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":30}}`, usageAdapterDeepSeek, 70, 10, 30, 0},
		{"deepseek-messages", "dual_protocol", "deepseek", "deepseek-chat", "/v1/messages", `{"input_tokens":20,"output_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":50}`, usageAdapterDeepSeek, 20, 10, 30, 50},
		// Explicit vendor patch applies to every model of the target (#160 F2).
		{"explicit-qwen-unlisted-model", "openai", "qwen", "unlisted-model", "/v1/chat/completions", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":30}}`, usageAdapterQwen, 70, 10, 30, 0},
		{"standard-unlisted-model", "openai", "standard", "unlisted-model", "/v1/chat/completions", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":30}}`, usageAdapterStandardOpenAI, 70, 10, 30, 0},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				path := tc.path
				payload := fmt.Sprintf(`{"model":%q,"usage":%s,"choices":[{"message":{"tool_calls":[{"id":"call-1","function":{"arguments":"{\"key\":1}"}}]},"finish_reason":"stop"}]}`, tc.model, tc.usage)
				protocol := ResolveProtocol(path)
				if protocol == config.ProtocolOpenAIResponses {
					payload = fmt.Sprintf(`{"id":"resp_matrix","object":"response","model":%q,"status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"f","arguments":"{}"}],"usage":%s}`, tc.model, tc.usage)
				}
				if protocol == config.ProtocolAnthropicMessages {
					payload = fmt.Sprintf(`{"type":"message","model":%q,"usage":%s,"content":[{"type":"thinking","thinking":"x","signature":"opaque-signature"}],"stop_reason":"end_turn"}`, tc.model, tc.usage)
				}
				if protocol == config.ProtocolGemini {
					payload = fmt.Sprintf(`{"modelVersion":%q,"usageMetadata":%s,"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"opaque-signature"}]},"finishReason":"STOP"}]}`, tc.model, tc.usage)
				}
				wire, ct := payload, "application/json"
				if stream {
					ct = "text/event-stream"
					switch protocol {
					case config.ProtocolAnthropicMessages:
						wire = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + payload + "}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":" + tc.usage + ",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					case config.ProtocolOpenAIResponses:
						wire = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
					case config.ProtocolGemini:
						path = strings.Replace(path, ":generateContent", ":streamGenerateContent?alt=sse", 1)
						wire = "data: " + payload + "\n\n"
					default:
						wire = "data: " + payload + "\n\ndata: [DONE]\n\n"
					}
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", ct); fmt.Fprint(w, wire) }))
				defer upstream.Close()
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5}, Targets: []config.Target{{Name: "upstream", Endpoint: upstream.URL, EndpointType: tc.endpointType, UsageAdapter: tc.adapter, APIKey: "upstream-key", SupportsResponses: true, ModelMappings: []config.ModelMapping{{Upstream: tc.model}}}}}
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
				request := fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, tc.model, stream)
				switch protocol {
				case config.ProtocolOpenAIResponses:
					request = fmt.Sprintf(`{"model":%q,"stream":%v,"input":"hi"}`, tc.model, stream)
				case config.ProtocolAnthropicMessages:
					request = fmt.Sprintf(`{"model":%q,"stream":%v,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, tc.model, stream)
				case config.ProtocolGemini:
					request = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
				}
				req := httptest.NewRequest("POST", path, strings.NewReader(request))
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
				if e.InputTokens != tc.in || e.OutputTokens != tc.out || e.CachedTokens != tc.cache || e.CacheCreationTokens != tc.write || e.UsageAdapter != tc.wantAdapter || e.UsageStatus != "complete" {
					t.Fatalf("bad usage: %+v", e)
				}
			})
		}
	}
}
