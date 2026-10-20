package proxy

import (
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"testing"
)

func TestMinimaxErrorPatch(t *testing.T) {
	for _, code := range []int{0, 1004, 2049, 1008, 2056, 1002, 1041, 2045, 2013, 9999} {
		body := []byte(fmt.Sprintf(`{"base_resp":{"status_code":%d}}`, code))
		for _, status := range []int{400, 401, 402, 429, 500} {
			got, r := targetKeyExhausted(&Target{UsageAdapter: "minimax"}, config.ProtocolOpenAIChat, status, body)
			want, reason := targetKeyExhausted(&Target{UsageAdapter: "standard"}, config.ProtocolOpenAIChat, status, body)
			if got != want || r != reason {
				t.Errorf("code=%d status=%d got %v %q want %v %q", code, status, got, r, want, reason)
			}
		}
		for _, adapter := range []string{"standard", "kimi"} {
			if got, _ := targetKeyExhausted(&Target{UsageAdapter: adapter}, config.ProtocolOpenAIChat, 400, body); got {
				t.Fatal(adapter, code)
			}
		}
	}
}
