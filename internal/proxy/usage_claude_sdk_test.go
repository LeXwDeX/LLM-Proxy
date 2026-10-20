package proxy

import (
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"testing"
)

func TestClaudeThinkingTokensSDKContract(t *testing.T) {
	for _, tc := range []struct {
		name, value       string
		want              int64
		present, conflict bool
	}{{"positive", "5", 5, true, false}, {"zero", "0", 0, true, false}, {"absent", "", 0, false, false}, {"negative", "-1", 0, true, true}, {"fraction", "1.5", 0, true, true}, {"overflow", "9223372036854775808", 0, true, true}, {"null", "null", 0, true, true}, {"exceeds_output", "21", 21, true, true}} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				details := ""
				if tc.value != "" {
					details = `,"output_tokens_details":{"thinking_tokens":` + tc.value + `}`
				}
				body := `{"usage":{"input_tokens":10,"output_tokens":20` + details + `}}`
				p := usageParser{protocol: config.ProtocolAnthropicMessages}
				var got usageTokens
				var found bool
				if stream {
					wire := `data: {"type":"message_start","message":` + body + `}` + "\n\n" + `data: {"type":"message_delta","usage":{"output_tokens":20}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
					got, _, found = p.extractSSE([]byte(wire))
				} else {
					got, _, found = p.extractTokens("application/json", []byte(body))
				}
				if !found || got.ReasoningTokens != tc.want || (got.fields&usageFieldReasoning != 0) != tc.present || (got.status == usageStatusConflict) != tc.conflict {
					t.Fatalf("got %+v found=%v", got, found)
				}
				if !tc.conflict {
					rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4}
					if rates.CostFor(got.InputTokens, got.OutputTokens, got.CachedTokens, got.CacheCreationTokens) != 0.0001 {
						t.Fatal("thinking double counted")
					}
				}
			})
		}
	}
}
func TestClaudeThinkingDeltaPresence(t *testing.T) {
	for _, tc := range []struct {
		name, delta string
		want        int64
	}{{"omitted", `"output_tokens":20`, 5}, {"zero", `"output_tokens":20,"output_tokens_details":{"thinking_tokens":0}`, 0}, {"corrected", `"output_tokens":20,"output_tokens_details":{"thinking_tokens":8}`, 8}} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0,"output_tokens_details":{"thinking_tokens":0}}}}` + "\n\n" + `data: {"type":"message_delta","usage":{"output_tokens":20,"output_tokens_details":{"thinking_tokens":5}}}` + "\n\n" + `data: {"type":"message_delta","usage":{` + tc.delta + `}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
			got, _, found := (usageParser{protocol: config.ProtocolAnthropicMessages}).extractSSE([]byte(wire))
			if !found || got.ReasoningTokens != tc.want || got.status != usageStatusComplete {
				t.Fatal(got)
			}
		})
	}
}
func TestClaudeServerToolChargesSDKContract(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
		incomplete  bool
	}{{"absent", "", false}, {"empty", `{}`, false}, {"zero", `{"web_fetch_requests":0,"web_search_requests":0}`, false}, {"fetch", `{"web_fetch_requests":1}`, true}, {"search", `{"web_search_requests":2}`, true}, {"unknown_zero", `{"future_requests":0}`, true}, {"unknown_positive", `{"future_requests":1}`, true}, {"negative", `{"web_search_requests":-1}`, true}, {"fraction", `{"web_search_requests":1.5}`, true}, {"overflow", `{"web_search_requests":9223372036854775808}`, true}, {"null_count", `{"web_search_requests":null}`, true}} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				tools := ""
				if tc.tools != "" {
					tools = `,"server_tool_use":` + tc.tools
				}
				body := `{"usage":{"input_tokens":10,"output_tokens":20` + tools + `}}`
				p := usageParser{protocol: config.ProtocolAnthropicMessages}
				var got usageTokens
				if stream {
					wire := `data: {"type":"message_start","message":` + body + `}` + "\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
					got, _, _ = p.extractSSE([]byte(wire))
				} else {
					got, _, _ = p.extractTokens("application/json", []byte(body))
				}
				if got.costIncomplete != tc.incomplete {
					t.Fatalf("got %+v", got)
				}
				if tc.incomplete && got.diagnostic == "" {
					t.Fatal("missing diagnostic")
				}
			})
		}
	}
}
