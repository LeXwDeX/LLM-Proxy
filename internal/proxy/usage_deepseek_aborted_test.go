package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// DeepSeek documents finish_reason=aborted ("生成过程被中断") for Chat
// Completions: https://api-docs.deepseek.com/zh-cn/api/create-chat-completion
// (checked 2026-10-20; documentation contract, not live acceptance).
// Only an explicitly selected deepseek adapter turns it into a failed request
// lifecycle. The response bytes, original reason and all reported usage are
// kept, and no retry, key switch, key disable or failover is added. standard
// and other vendors keep the original-protocol behavior (counterexample).
func TestDeepSeekAbortedFinishLifecycleIsAdapterScoped(t *testing.T) {
	jsonBody := `{"id":"c","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"part"},"finish_reason":"aborted"}],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":40},"prompt_cache_hit_tokens":40,"prompt_cache_miss_tokens":60}}`
	sseBody := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"aborted\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"total_tokens\":107,\"prompt_tokens_details\":{\"cached_tokens\":40},\"prompt_cache_hit_tokens\":40,\"prompt_cache_miss_tokens\":60}}\n\n" +
		"data: [DONE]\n\n"
	for _, adapter := range []string{"deepseek", "standard", "glm", "kimi", "qwen", "openrouter", "minimax"} {
		for _, stream := range []bool{false, true} {
			name := adapter + "/json"
			typ, body := "application/json", jsonBody
			if stream {
				name, typ, body = adapter+"/sse", "text/event-stream", sseBody
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", typ)
					_, _ = io.WriteString(w, body)
				}))
				defer up.Close()
				s := newTestModelService(t, config.EndpointTypeOpenAI, up.URL, func(tg *config.Target) {
					tg.UsageAdapter = adapter
					tg.APIKeys = []string{"second"}
				})
				defer s.Close()
				stats := &completionRecorder{}
				s.SetRequestStatsRecorder(stats)
				billing := &adapterEventRecorder{}
				s.SetUsageRecorder(billing)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, statsRequest(t, "/v1/chat/completions"))
				if w.Code != 200 || w.Body.String() != body {
					t.Fatalf("response bytes changed: %d %q", w.Code, w.Body.String())
				}
				want := requeststats.Success
				if adapter == "deepseek" {
					want = requeststats.Failure
				}
				assertCompletions(t, stats, want)
				if len(billing.events) != 1 {
					t.Fatalf("usage events=%d", len(billing.events))
				}
				e := billing.events[0]
				if (e.ResponseStatus == "failed") != (adapter == "deepseek") || e.FinishReason != "aborted" {
					t.Fatalf("response_status=%q finish_reason=%q", e.ResponseStatus, e.FinishReason)
				}
				if e.UsageStatus != usageStatusComplete || e.CostIncomplete || e.InputTokens+e.CachedTokens != 100 || e.CachedTokens != 40 || e.OutputTokens != 7 {
					t.Fatalf("usage not preserved exactly: %+v", e)
				}
				state := s.targetOrder[0]
				metrics := s.MetricsSnapshot()
				if calls.Load() != 1 || metrics.TotalRetries != 0 || metrics.TotalKeyRetries != 0 || metrics.TotalTargetRetries != 0 {
					t.Fatalf("aborted finish replayed: calls=%d metrics=%+v", calls.Load(), metrics)
				}
				for i := range state.keyPool.entries {
					if reason := state.keyPool.exhaustReasonAt(i); reason != "" {
						t.Fatalf("key %d disabled by aborted finish: %q", i, reason)
					}
				}
			})
		}
	}
}
