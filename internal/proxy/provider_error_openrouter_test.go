package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"testing"
)

func TestOpenRouterErrorPatch(t *testing.T) {
	for _, tc := range []struct {
		status       int
		body, reason string
	}{
		{402, `{"error":{"metadata":{"limit_source":"openrouter_in_flight_budget"}}}`, "rate_limited"},
		{402, `{"error":{"metadata":{"reason":"in_flight_budget_exhausted"}}}`, "rate_limited"},
		{402, `{"error":{"metadata":{"limit_source":"openrouter_credits","reason":"in_flight_budget_exhausted"}}}`, "billing_error"},
		{402, `{"error":{"metadata":{"reason":"weight_exceeds_budget"}}}`, ""},
		{402, `{}`, "billing_error"},
		{403, `{"error":{"metadata":{"limit_source":"openrouter_key_limit"}}}`, "quota_exceeded"},
		{403, `{"error":{"metadata":{"limit_source":"openrouter_guardrail_budget"}}}`, "quota_exceeded"},
		{403, `{"error":{"metadata":{"limit_source":"openrouter_workspace_budget"}}}`, "quota_exceeded"},
		{403, `{"error":{"message":"openrouter_key_limit"}}`, ""},
		{403, `{"error":{"metadata":{"reasons":["moderation"],"flagged_input":true}}}`, ""},
		{401, `{}`, "invalid_token"},
		{429, `{}`, ""}, {408, `{}`, ""}, {500, `{}`, ""},
		{200, `{"error":{"metadata":{"limit_source":"openrouter_key_limit"}}}`, ""},
	} {
		got, reason := targetKeyExhausted(&Target{UsageAdapter: "openrouter"}, config.ProtocolOpenAIChat, tc.status, []byte(tc.body))
		if got != (tc.reason != "") || reason != tc.reason {
			t.Errorf("%d %s: %v %q want %q", tc.status, tc.body, got, reason, tc.reason)
		}
	}
	for _, adapter := range []string{"standard", "kimi"} {
		if ok, _ := targetKeyExhausted(&Target{UsageAdapter: adapter}, config.ProtocolOpenAIChat, 403, []byte(`{"error":{"metadata":{"limit_source":"openrouter_key_limit"}}}`)); ok {
			t.Fatal(adapter)
		}
	}
}
