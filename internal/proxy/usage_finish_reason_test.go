package proxy

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Vendor-specific aborts require an explicit selection. JSON and SSE keep the
// original reason and all reported usage whether or not it is a failure.
func TestVendorFailureFinishReasonMarksFailedButKeepsUsage(t *testing.T) {
	for _, adapter := range []string{"standard", "qwen", "glm", "deepseek", "kimi", "openrouter", "minimax"} {
		for _, reason := range []string{"error", "network_error", "sensitive", "insufficient_system_resource", "aborted", "stop", "length", "tool_calls", "content_filter"} {
			for _, stream := range []bool{false, true} {
				name := adapter + "/" + reason + "/json"
				if stream {
					name = adapter + "/" + reason + "/sse"
				}
				t.Run(name, func(t *testing.T) {
					parser := usageParser{protocol: config.ProtocolOpenAIChat, usageAdapter: adapter, model: "unlisted-model"}
					body := `{"choices":[{"finish_reason":"` + reason + `"}],"usage":{"prompt_tokens":100,"completion_tokens":7}}`
					var got usageTokens
					var found bool
					if stream {
						body = "data: {\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\n" + "data: " + body + "\n\ndata: [DONE]\n\n"
						got, _, found = parser.extractSSE([]byte(body))
					} else {
						got, _, found = parser.extractTokens("application/json", []byte(body))
					}
					wantFailed := reason == "error" || (adapter == "glm" && (reason == "network_error" || reason == "sensitive")) || (adapter == "deepseek" && (reason == "insufficient_system_resource" || reason == "aborted"))
					if (got.responseStatus == "failed") != wantFailed || got.finishReason != reason {
						t.Fatalf("status=%q reason=%q wantFailed=%v", got.responseStatus, got.finishReason, wantFailed)
					}
					if !found || got.status != usageStatusComplete || got.InputTokens != 100 || got.OutputTokens != 7 {
						t.Fatalf("usage lost: %+v found=%v", got, found)
					}
				})
			}
		}
	}
}
