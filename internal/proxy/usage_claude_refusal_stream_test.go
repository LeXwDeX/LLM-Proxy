package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestClaudeRefusalBillingAllShapes(t *testing.T) {
	for _, shape := range []string{"ordinary", "message", "compaction", "advisor", "fallback"} {
		for _, tc := range []struct {
			name, details, stop string
			output              int
			free, incomplete    bool
		}{
			{"cyber", `{"category":"cyber"}`, "refusal", 0, true, false},
			{"general_harms", `{"category":"general_harms"}`, "refusal", 0, true, false},
			{"null", `{"category":null}`, "refusal", 0, true, false},
			{"bio", `{"category":"bio"}`, "refusal", 0, false, false},
			{"frontier", `{"category":"frontier_llm"}`, "refusal", 0, false, false},
			{"reasoning", `{"category":"reasoning_extraction"}`, "refusal", 0, false, false},
			{"unknown", `{"category":"future_policy"}`, "refusal", 0, true, true},
			{"missing", `{}`, "refusal", 0, true, true},
			{"invalid", `{"category":42}`, "refusal", 0, true, true},
			{"nonzero", `{"category":"cyber"}`, "refusal", 2, false, false},
			{"not_refusal", `{"category":"cyber"}`, "end_turn", 0, false, false},
		} {
			for _, adapter := range []string{"standard", "qwen"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/stream=%v", shape, tc.name, adapter, stream), func(t *testing.T) {
						counts := fmt.Sprintf(`"input_tokens":7,"output_tokens":%d`, tc.output)
						items := ""
						children := 0
						switch shape {
						case "message":
							items = `,{"type":"message",` + counts + `}`
						case "compaction", "advisor":
							typ := "compaction"
							if shape == "advisor" {
								typ = "advisor_message"
							}
							items = `,{"type":"message",` + counts + `},{"type":"` + typ + `","model":"extra","input_tokens":11,"output_tokens":2}`
							children = 1
						case "fallback":
							items = `,{"type":"message","input_tokens":11,"output_tokens":2},{"type":"fallback_message","model":"fallback",` + counts + `}`
							children = 1
						}
						u := `{` + counts
						if items != "" {
							u += `,"iterations":[` + items[1:] + `]`
						}
						u += `}`
						stop := fmt.Sprintf(`"stop_reason":%q,"stop_details":%s`, tc.stop, tc.details)
						body, ct := `{`+stop+`,"usage":`+u+`}`, "application/json"
						if stream {
							ct = "text/event-stream"
							body = `data: {"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" + `data: {"type":"message_delta","delta":{` + stop + `},"usage":` + u + `}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
						}
						p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor", usageAdapter: adapter}
						tokens, _, found := p.extractTokens(ct, []byte(body))
						if !found || tokens.nonBillable != tc.free || tokens.costIncomplete != tc.incomplete || len(tokens.components) != children || tokens.InputTokens != 7 || tokens.OutputTokens != int64(tc.output) {
							t.Fatalf("unexpected refusal billing: %+v", tokens)
						}
						if tc.incomplete && !strings.Contains(tokens.diagnostic, "refusal_billing") {
							t.Fatalf("diagnostic=%q", tokens.diagnostic)
						}
						for _, child := range tokens.components {
							if child.NonBillable || child.CostIncomplete || child.InputTokens != 11 || child.OutputTokens != 2 {
								t.Fatalf("root refusal affected child: %+v", child)
							}
						}
					})
				}
			}
		}
	}
}

func TestClaudeRefusalRequiresObservedZeroOutput(t *testing.T) {
	for _, output := range []string{"", `,"output_tokens":null`, `,"output_tokens":-1`, `,"output_tokens":"0"`} {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fallback=%v", output, fallback), func(t *testing.T) {
				items := ""
				if fallback {
					items = `,"iterations":[{"type":"fallback_message","model":"fallback","input_tokens":7,"output_tokens":0}]`
				}
				p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
				_, e := claudeBillingEvent(t, p, `{"stop_reason":"refusal","stop_details":{"category":"cyber"},"usage":{"input_tokens":7`+output+items+`}}`, "application/json")
				if e.NonBillable || !e.CostIncomplete {
					t.Fatalf("unobserved zero granted exemption: %+v", e)
				}
			})
		}
	}
}

func TestClaudeStreamIterationsRequireFinalSnapshot(t *testing.T) {
	const counts = `"input_tokens":7,"output_tokens":2`
	const items = `[{"type":"message",` + counts + `},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1}]`
	for _, earlyType := range []string{"message_start", "message_delta"} {
		for _, finalItems := range []string{"", `,"iterations":null`, `,"iterations":[]`, `,"iterations":` + items} {
			t.Run(earlyType+"/"+finalItems, func(t *testing.T) {
				early := `data: {"type":"message_delta","delta":{},"usage":{` + counts + `,"iterations":` + items + `}}` + "\n\n"
				if earlyType == "message_start" {
					early = `data: {"type":"message_start","message":{"usage":{` + counts + `,"iterations":` + items + `}}}` + "\n\n"
				}
				wire := early + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + counts + finalItems + `}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
				p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
				_, e := claudeBillingEvent(t, p, wire, "text/event-stream")
				missing := finalItems == "" || finalItems == `,"iterations":null`
				children := 0
				if finalItems == `,"iterations":`+items {
					children = 1
				}
				if e.CostIncomplete != missing || (missing && !strings.Contains(e.UsageDiagnostic, "missing_final_iterations")) || len(e.Components) != children || e.InputTokens != 7 || e.OutputTokens != 2 {
					t.Fatalf("lost or double charged evidence: %+v", e)
				}
			})
		}
	}
}

func TestClaudeStreamIntermediateOutputIsNotFinal(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	u := newUsageStream("text/event-stream", "", p)
	u.Write([]byte(`data: {"type":"message_delta","delta":{},"usage":{"input_tokens":7,"output_tokens":2,"iterations":[{"type":"message","input_tokens":7,"output_tokens":2},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1}]}}` + "\n\n"))
	u.Close()
	if u.finalSeen || len(u.tokens.components) != 0 {
		t.Fatalf("intermediate delta billed as final: %+v", u.tokens)
	}
}

func TestClaudeFallbackMissingInputHasDedicatedDiagnostic(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor", requestBody: []byte(`{"service_tier":"priority","speed":"fast"}`)}
	wire := `data: {"type":"message_start","message":{"model":"executor","usage":{"input_tokens":999,"output_tokens":1,"cache_read_input_tokens":99}}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"iterations":[{"type":"message","model":"executor","input_tokens":13,"output_tokens":4},{"type":"fallback_message","model":"fallback","input_tokens":7,"output_tokens":2}]}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
	_, e := claudeBillingEvent(t, p, wire, "text/event-stream")
	if !e.CostIncomplete || !strings.Contains(e.UsageDiagnostic, "missing_fallback_input_usage") || e.InputTokens != 0 || e.CachedTokens != 0 || len(e.Components) != 0 || e.ObservedFields&usageFieldInput != 0 || e.Model != "fallback" {
		t.Fatalf("fallback inherited executor or lost diagnostic: %+v", e)
	}
	if e.PricingContext == nil || e.PricingContext.RequestServiceTier.State != "missing" || e.PricingContext.RequestSpeed.State != "missing" {
		t.Fatalf("partial fallback inherited executor pricing: %+v", e.PricingContext)
	}
}

func TestClaudeStreamErrorCannotPromoteIntermediateUsage(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages}
	u := newUsageStream("text/event-stream", "", p)
	u.Write([]byte("data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}\n\ndata: {\"type\":\"error\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	u.Close()
	if u.finalSeen {
		t.Fatal("error invalidated usage promoted by message_stop")
	}
}

func TestClaudeStreamStopConfirmsEventOnlyDelta(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	wire := "event: message_delta\ndata: {\"delta\":{},\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"iterations\":[{\"type\":\"message\",\"input_tokens\":7,\"output_tokens\":2},{\"type\":\"advisor_message\",\"model\":\"advisor\",\"input_tokens\":3,\"output_tokens\":1}]}}\n\nevent: message_stop\ndata: {}\n\n"
	u := newUsageStream("text/event-stream", "", p)
	u.Write([]byte(wire))
	u.Close()
	if !u.finalSeen || u.tokens.costIncomplete || len(u.tokens.components) != 1 {
		t.Fatalf("terminal did not confirm last delta: %+v", u.tokens)
	}
}

func TestClaudePartialFallbackAttributionRequiresValidFinalAttempt(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	for _, item := range []string{
		`{"type":"fallback_message","model":"fallback","input_tokens":7,"output_tokens":3}`,
		`{"type":"fallback_message","model":"","input_tokens":7,"output_tokens":2}`,
		`{"type":"fallback_message","model":2,"input_tokens":7,"output_tokens":2}`,
		`{"type":"fallback_message","model":"fallback","output_tokens":2}`,
		`{"type":"message","model":"fallback","input_tokens":7,"output_tokens":2}`,
	} {
		t.Run(item, func(t *testing.T) {
			tokens, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"output_tokens":2,"iterations":[`+item+`]}}`))
			if tokens.billingModel != "" || !tokens.costIncomplete || len(tokens.components) != 0 {
				t.Fatalf("unproven partial attribution: %+v", tokens)
			}
		})
	}
}
