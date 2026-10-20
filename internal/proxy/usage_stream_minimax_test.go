package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"strings"
	"testing"
)

func TestMinimaxUsageStreamProjectionAndStickyFailure(t *testing.T) {
	for _, adapter := range []string{"minimax", "standard"} {
		for _, body := range []string{
			`{"base_resp":{"status_code":2013},"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			"data: {\"base_resp\":{\"status_code\":2013}}\n\ndata: {\"status\":\"completed\",\"base_resp\":{\"status_code\":0},\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n",
		} {
			u := newUsageStream("application/json", "", usageParser{protocol: config.ProtocolOpenAIChat, usageAdapter: adapter})
			u.Write([]byte(body))
			u.Close()
			if u.err != nil || !u.found || u.tokens.InputTokens != 7 || u.tokens.OutputTokens != 3 {
				t.Fatalf("usage lost: %+v err=%v", u.tokens, u.err)
			}
			if (u.responseStatus == "failed") != (adapter == "minimax" && !strings.HasPrefix(body, "data:")) {
				t.Fatalf("%s: status=%s", adapter, u.responseStatus)
			}
		}
	}
}
