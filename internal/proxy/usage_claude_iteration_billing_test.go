package proxy

import (
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func claudeBillingEvent(t *testing.T, p usageParser, wire, contentType string) (usageTokens, usage.Event) {
	t.Helper()
	tokens, model, found := p.extractTokens(contentType, []byte(wire))
	if !found {
		t.Fatalf("usage not observed: %+v", tokens)
	}
	return tokens, claudeRecordTokens(t, p, tokens, model)
}

func claudeRecordTokens(t *testing.T, p usageParser, tokens usageTokens, model string) usage.Event {
	t.Helper()
	recorder := &adapterEventRecorder{}
	s := &Service{}
	s.SetUsageRecorder(recorder)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "iterations-client")))
	s.recordParsedUsage(req, &Target{Name: "iterations-target", EndpointType: config.EndpointTypeClaude}, 200, p.model, tokens, model, nil, -1)
	if len(recorder.events) != 1 {
		t.Fatalf("records=%d; one bundle required", len(recorder.events))
	}
	return recorder.events[0]
}

func TestClaudeIterationBillingComponents(t *testing.T) {
	const top = `"input_tokens":70,"output_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":0`
	const message = `{"type":"message",` + top + `}`
	for _, tc := range []struct {
		name, top, items, rootModel, childModel, typ string
		in, out                                      int64
	}{
		{"advisor", top, message + `,{"type":"advisor_message","model":"Advisor-M","input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, "executor-m", "advisor-m", "advisor_message", 7, 3},
		{"compaction", top, `{"type":"compaction","input_tokens":11,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},` + message, "executor-m", "executor-m", "compaction", 11, 2},
		{"on_demand", `"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0`, `{"type":"compaction","input_tokens":11,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, "executor-m", "executor-m", "compaction", 11, 2},
		{"fallback", top, `{"type":"message","input_tokens":13,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"fallback_message","model":"Fallback-M",` + top + `}`, "fallback-m", "executor-m", "message", 13, 4},
	} {
		for _, adapter := range []string{"standard", "qwen"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.name, adapter, stream), func(t *testing.T) {
					p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "Executor-M", usageAdapter: adapter, requestBody: []byte(`{"service_tier":"priority","speed":"fast"}`)}
					u := `{` + tc.top + `,"service_tier":"priority","speed":"fast","iterations":[` + tc.items + `]}`
					wire := `{"model":"Executor-M","stop_reason":"end_turn","usage":` + u + `}`
					ct := "application/json"
					if stream {
						ct = "text/event-stream"
						wire = `data: {"type":"message_start","message":{"model":"Executor-M","usage":{"input_tokens":999,"output_tokens":1}}}` + "\n\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + u + `}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					tokens, event := claudeBillingEvent(t, p, wire, ct)
					if tokens.costIncomplete || tokens.status != usageStatusComplete {
						t.Fatalf("parser incomplete: %+v", tokens)
					}
					if event.Model != tc.rootModel || len(event.Components) != 1 {
						t.Fatalf("wrong bundle: %+v", event)
					}
					child := event.Components[0]
					if child.Model != tc.childModel || child.IterationType != tc.typ || child.InputTokens != tc.in || child.OutputTokens != tc.out {
						t.Fatalf("wrong component: %+v", child)
					}
					if child.PricingContext == nil || child.PricingContext.TotalInputTokens != tc.in || !child.PricingContext.TotalInputValid || child.PricingContext.ActualServiceTier != "" || child.PricingContext.ActualSpeed != "" || child.PricingContext.RequestServiceTier.State != usage.PricingValueMissing || child.PricingContext.RequestSpeed.State != usage.PricingValueMissing {
						t.Fatalf("component borrowed executor context: %+v", child.PricingContext)
					}
					if tc.name == "fallback" && (event.IterationType != "fallback_message" || event.IterationIndex != 1) {
						t.Fatalf("returned attempt attribution missing: %+v", event)
					}
				})
			}
		}
	}
}

func TestClaudeFallbackRefusalBilling(t *testing.T) {
	for _, tc := range []struct {
		name, details    string
		free, incomplete bool
	}{
		{"paid_bio", `"stop_details":{"category":"bio"}`, false, false},
		{"paid_frontier", `"stop_details":{"category":"frontier_llm"}`, false, false},
		{"paid_reasoning", `"stop_details":{"category":"reasoning_extraction"}`, false, false},
		{"free_other", `"stop_details":{"category":"cyber"}`, true, false},
		{"free_null", `"stop_details":{"category":null}`, true, false},
		{"missing", `"stop_details":{}`, true, true},
		{"missing_object", `"irrelevant":null`, true, true},
		{"invalid", `"stop_details":{"category":3}`, true, true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "Executor-M"}
				u := `{"input_tokens":7,"output_tokens":0,"iterations":[{"type":"fallback_message","model":"Fallback-M","input_tokens":7,"output_tokens":0}]}`
				body := `{"model":"Executor-M","stop_reason":"refusal",` + tc.details + `,"usage":` + u + `}`
				ct := "application/json"
				if stream {
					ct = "text/event-stream"
					body = `data: {"type":"message_start","message":{"model":"Executor-M","usage":{"input_tokens":999,"output_tokens":1}}}` + "\n\n" + `data: {"type":"message_delta","delta":{"stop_reason":"refusal",` + tc.details + `},"usage":` + u + `}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				tokens, event := claudeBillingEvent(t, p, body, ct)
				if event.Model != "fallback-m" || !strings.EqualFold(event.ResponseModel, "Fallback-M") || event.NonBillable != tc.free || tokens.costIncomplete != tc.incomplete || len(event.Components) != 0 || event.IterationType != "fallback_message" {
					t.Fatalf("tokens=%+v event=%+v", tokens, event)
				}
				if tc.incomplete && tokens.diagnostic != "unknown_fallback_refusal_billing" {
					t.Fatalf("diagnostic=%s", tokens.diagnostic)
				}
			})
		}
	}
}

func TestClaudeEarlierZeroFallbackHasIndependentBillingEvidence(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "Executor-M"}
	body := `{"stop_reason":"refusal","stop_details":{"category":"bio"},"usage":{"input_tokens":7,"output_tokens":0,"iterations":[{"type":"message","input_tokens":13,"output_tokens":0},{"type":"fallback_message","model":"Fallback-M","input_tokens":7,"output_tokens":0}]}}`
	tokens, event := claudeBillingEvent(t, p, body, "application/json")
	if event.NonBillable || !tokens.costIncomplete || tokens.diagnostic != "unknown_fallback_refusal_billing" || len(event.Components) != 1 {
		t.Fatalf("wrong root: %+v %+v", tokens, event)
	}
	child := event.Components[0]
	if !child.NonBillable || !child.CostIncomplete || child.UsageDiagnostic != "unknown_fallback_refusal_billing" || child.InputTokens != 13 {
		t.Fatalf("prior attempt incorrectly used final category: %+v", child)
	}
}

func TestClaudeIterationComponentMissingCacheStaysUnobserved(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "Executor-M"}
	_, event := claudeBillingEvent(t, p, `{"usage":{"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction","input_tokens":11,"output_tokens":2}]}}`, "application/json")
	if len(event.Components) != 1 {
		t.Fatalf("component missing: %+v", event)
	}
	child := event.Components[0]
	if child.ObservedFields&(usage.UsageFieldCached|usage.UsageFieldCacheCreation) != 0 || child.PricingContext == nil || child.PricingContext.TotalInputValid {
		t.Fatalf("missing cache inferred as zero: %+v", child)
	}
}

func TestClaudeIterationBillingRejectsUnknownAttribution(t *testing.T) {
	const top = `"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0`
	const executor = `{"type":"message",` + top + `}`
	for _, item := range []string{
		`{"type":"advisor_message","input_tokens":1,"output_tokens":1}`,
		`{"type":"advisor_message","model":"","input_tokens":1,"output_tokens":1}`,
		`{"type":"advisor_message","model":" ","input_tokens":1,"output_tokens":1}`,
		`{"type":"advisor_message","model":12,"input_tokens":1,"output_tokens":1}`,
		`{"type":"advisor_message","model":"a","input_tokens":-1,"output_tokens":1}`,
		`{"type":"advisor_message","model":"a","input_tokens":1.5,"output_tokens":1}`,
		`{"type":"advisor_message","model":"a","input_tokens":1}`,
		`{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1,"cache_creation":false}`,
		`{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1,"output_tokens_details":{"thinking_tokens":2}}`,
		`{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":3}}`,
		`{"type":"advisor_message","model":"a","input_tokens":9223372036854775807,"output_tokens":1,"cache_read_input_tokens":1}`,
		`{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1,"iterations":[]}`,
		`{"type":"unknown","model":"a","input_tokens":1,"output_tokens":1}`,
		`{"input_tokens":1,"output_tokens":1}`, `false`,
	} {
		t.Run(item, func(t *testing.T) {
			p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
			tokens, event := claudeBillingEvent(t, p, `{"usage":{`+top+`,"iterations":[`+executor+`,`+item+`]}}`, "application/json")
			if len(event.Components) != 0 || !tokens.costIncomplete || event.InputTokens != 7 || event.OutputTokens != 2 {
				t.Fatalf("unproved attribution billed: %+v %+v", tokens, event)
			}
		})
	}
}

func TestClaudeIterationBillingBundleOverflow(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	tokens, event := claudeBillingEvent(t, p, `{"usage":{"input_tokens":9223372036854775807,"output_tokens":0,"iterations":[{"type":"message","input_tokens":9223372036854775807,"output_tokens":0},{"type":"advisor_message","model":"advisor","input_tokens":1,"output_tokens":0}]}}`, "application/json")
	if len(event.Components) != 0 || !tokens.costIncomplete {
		t.Fatalf("overflowing bundle accepted: %+v %+v", tokens, event)
	}
}

func TestClaudeIterationStreamSnapshotsAndSettlement(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	const top = `"input_tokens":7,"output_tokens":2`
	const items = `[{"type":"message",` + top + `},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1}]`
	early := `data: {"type":"message_start","message":{"usage":{` + top + `,"iterations":` + items + `}}}` + "\n\n"
	final := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + top + `,"iterations":` + items + `}}` + "\n\n"
	for _, tc := range []struct {
		name, wire string
		children   int
	}{
		{"early_only", early + `data: {"type":"message_stop"}` + "\n\n", 0},
		{"early_then_absent", early + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + top + `}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n", 0},
		{"two_final_snapshots", early + final + final + `data: {"type":"message_stop"}` + "\n\n", 1},
		{"final_empty_replaces", early + final + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + top + `,"iterations":[]}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, event := claudeBillingEvent(t, p, tc.wire, "text/event-stream")
			if len(event.Components) != tc.children {
				t.Fatalf("stale or duplicate components: %+v", event)
			}
		})
	}
	for _, tc := range []struct {
		name, wire string
		hasFinal   bool
	}{
		{"before_final", early, false},
		{"after_final", early + final, true},
		{"error_invalidates", early + final + `data: {"type":"error","error":{"type":"overloaded_error"}}` + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := newUsageStream("text/event-stream", "", p)
			observer.Write([]byte(tc.wire))
			observer.Close()
			s := &Service{}
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			tokens, model := s.settleNoOfficialUsage(req, nil, noOfficialUsage{cause: noUsageCauseClientGone, usage: observer, tokens: observer.tokens, quiet: true})
			event := claudeRecordTokens(t, p, tokens, model)
			if tc.hasFinal {
				if len(event.Components) != 1 || event.InputTokens != 7 {
					t.Fatalf("official final lost: %+v", event)
				}
			} else if len(event.Components) != 0 || event.InputTokens != 0 || event.OutputTokens != 0 || event.PricingContext != nil {
				t.Fatalf("unofficial components survived clearing: %+v", event)
			}
		})
	}
}

func TestClaudeFallbackDoesNotInheritExecutorServiceTier(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor", requestBody: []byte(`{"service_tier":"priority","speed":"fast"}`)}
	wire := `data: {"type":"message_start","message":{"model":"executor","usage":{"input_tokens":7,"output_tokens":1,"service_tier":"priority","speed":"fast"}}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":9,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"type":"fallback_message","model":"fallback","input_tokens":9,"output_tokens":2}]}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
	_, event := claudeBillingEvent(t, p, wire, "text/event-stream")
	c := event.PricingContext
	if c == nil || c.TotalInputTokens != 9 || !c.TotalInputValid || c.ActualServiceTier != "" || c.ActualSpeed != "" || c.RequestServiceTier.State != usage.PricingValueMissing || c.RequestSpeed.State != usage.PricingValueMissing {
		t.Fatalf("fallback inherited executor context: %+v", c)
	}
}

func TestClaudeStopDetailsProjectionDiscardsExplanation(t *testing.T) {
	payload, err := readUsagePayload(strings.NewReader(`{"stop_details":{"category":null,"explanation":"bio","content":[{"category":"reasoning_extraction"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	details, ok := payload["stop_details"].(map[string]any)
	if !ok || len(details) != 1 {
		t.Fatalf("unexpected stop detail projection: %+v", payload)
	}
	category, present := details["category"]
	if !present || category != nil {
		t.Fatalf("null lost: %+v", details)
	}
}

func TestClaudeFallbackChainBillsEveryAttempt(t *testing.T) {
	for _, firstOutput := range []int{4, 0} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("first_output=%d/stream=%v", firstOutput, stream), func(t *testing.T) {
				p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "Executor-M"}
				u := fmt.Sprintf(`{"input_tokens":7,"output_tokens":3,"iterations":[{"type":"message","model":"Executor-M","input_tokens":13,"output_tokens":%d},{"type":"message","model":"Middle-M","input_tokens":5,"output_tokens":2},{"type":"fallback_message","model":"Fallback-M","input_tokens":7,"output_tokens":3}]}`, firstOutput)
				body := `{"model":"Executor-M","stop_reason":"end_turn","usage":` + u + `}`
				ct := "application/json"
				if stream {
					ct = "text/event-stream"
					body = `data: {"type":"message_start","message":{"model":"Executor-M","usage":{"input_tokens":999,"output_tokens":1}}}` + "\n\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + u + `}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				tokens, event := claudeBillingEvent(t, p, body, ct)
				if event.Model != "fallback-m" || event.IterationIndex != 2 || len(event.Components) != 2 {
					t.Fatalf("multi-hop attribution lost: %+v", event)
				}
				first, second := event.Components[0], event.Components[1]
				if first.Model != "executor-m" || first.IterationIndex != 0 || first.InputTokens != 13 || first.OutputTokens != int64(firstOutput) || first.NonBillable != (firstOutput == 0) || second.Model != "middle-m" || second.IterationIndex != 1 || second.InputTokens != 5 || second.OutputTokens != 2 || second.NonBillable {
					t.Fatalf("wrong chain components: %+v", event.Components)
				}
				if tokens.costIncomplete != (firstOutput == 0) || (firstOutput == 0 && tokens.diagnostic != "unknown_fallback_refusal_billing") {
					t.Fatalf("wrong chain billing evidence: %+v", tokens)
				}
			})
		}
	}
}

func TestClaudeFallbackFinalUsageDoesNotInheritExecutorCounters(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	const final = `{"input_tokens":7,"output_tokens":2,"iterations":[{"type":"fallback_message","model":"fallback","input_tokens":7,"output_tokens":2}]}`
	_, expected := claudeBillingEvent(t, p, `{"model":"fallback","stop_reason":"end_turn","usage":`+final+`}`, "application/json")
	wire := `data: {"type":"message_start","message":{"model":"executor","usage":{"input_tokens":99,"output_tokens":1,"cache_read_input_tokens":999,"cache_creation_input_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":9},"output_tokens_details":{"thinking_tokens":1}}}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + final + `}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
	_, event := claudeBillingEvent(t, p, wire, "text/event-stream")
	if event.Model != expected.Model || event.InputTokens != expected.InputTokens || event.OutputTokens != expected.OutputTokens || event.CachedTokens != 0 || event.CacheCreationTokens != 0 || event.CacheCreation5mTokens != 0 || event.ReasoningTokens != 0 || event.ObservedFields != expected.ObservedFields || event.PricingContext.TotalInputValid {
		t.Fatalf("fallback borrowed old model counters: %+v; JSON %+v", event, expected)
	}
}

func TestIterationBundleCountsKeyWorkloadOnce(t *testing.T) {
	p := usageParser{protocol: config.ProtocolAnthropicMessages, model: "executor"}
	compact, _, _ := p.extractTokens("application/json", []byte(`{"usage":{"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction","input_tokens":11,"output_tokens":2}]}}`))
	for _, tc := range []struct {
		name   string
		tokens usageTokens
		want   int64
	}{
		{"ordinary", usageTokens{InputTokens: 2, OutputTokens: 3, CachedTokens: 4, CacheCreationTokens: 5, ToolUsePromptTokens: 6, ReasoningTokens: 3, CacheCreation5mTokens: 5}, 20},
		{"on_demand_compaction", compact, 13},
		{"non_billable_is_work", usageTokens{InputTokens: 2, OutputTokens: 3, nonBillable: true, components: []usage.Component{{InputTokens: 7, OutputTokens: 5, CachedTokens: 3, CacheCreationTokens: 2, ToolUsePromptTokens: 1, ReasoningTokens: 4, CacheCreation5mTokens: 2, NonBillable: true}}}, 23},
		{"saturates", usageTokens{InputTokens: math.MaxInt64 - 1, components: []usage.Component{{InputTokens: 2}}}, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestKeyPool([]string{"fake-work-0", "fake-work-1"})
			pool.recordTokens(1, tc.want-1)
			s := &Service{}
			s.SetUsageRecorder(&adapterEventRecorder{})
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			req = req.WithContext(auth.WithPrincipal(req.Context(), newAllowAllPrincipal(t, "workload-client")))
			s.recordParsedUsage(req, &Target{Name: "workload", EndpointType: config.EndpointTypeClaude}, 200, "executor", tc.tokens, "executor", &targetState{keyPool: pool}, 0)
			pool.mu.Lock()
			got := pool.tokenSumInWindow(0, time.Now())
			pool.mu.Unlock()
			if got != tc.want {
				t.Fatalf("recorded workload=%d want %d", got, tc.want)
			}
			_, index := pool.selectKeyForClient("new-workload-client")
			if index != 1 {
				t.Fatalf("key %d selected despite higher observed workload", index)
			}
		})
	}
}
