package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
)

// Exercise the real response observer and Service settlement, not just the parser.
func TestClaudeErrorSettlementPreservesConfirmedUsage(t *testing.T) {
	const stop = "data: {\"type\":\"message_stop\"}\n\n"
	const streamError = "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"synthetic late error\"}}\n\n"
	for _, tc := range []struct {
		name, reason, beforeError, afterError string
		confirmed, free, fallback             bool
	}{
		{name: "intermediate_error"},
		{name: "intermediate_error_stop", afterError: stop},
		{name: "final_delta_error", reason: `"stop_reason":"end_turn"`, confirmed: true},
		{name: "final_delta_error_stop", reason: `"stop_reason":"end_turn"`, afterError: stop, confirmed: true},
		{name: "message_stop_confirmed_error", beforeError: stop, confirmed: true},
		{name: "free_root_paid_advisor_error", reason: `"stop_reason":"refusal","stop_details":{"category":"cyber"}`, confirmed: true, free: true},
		{name: "fallback_error", reason: `"stop_reason":"end_turn"`, confirmed: true, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := 2
			if tc.free {
				output = 0
			}
			counts := fmt.Sprintf(`"input_tokens":7,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0`, output)
			items := `[{"type":"message",` + counts + `},{"type":"advisor_message","model":"advisor","input_tokens":3,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]`
			rootModel, childModel := "test-model", "advisor"
			if tc.fallback {
				rootModel, childModel = "fallback", "test-model"
				items = `[{"type":"message","input_tokens":3,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"fallback_message","model":"fallback",` + counts + `}]`
			}
			wire := `data: {"type":"message_start","message":{"model":"test-model","usage":{"input_tokens":99,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{` + tc.reason + `},"usage":{` + counts + `,"iterations":` + items + `}}` + "\n\n" + tc.beforeError + streamError + tc.afterError
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			}))
			defer up.Close()
			s, events, stats, _ := goneService(t, up.URL, config.EndpointTypeClaude, config.UsageAdapterStandard)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, goneRequest(t, "/v1/messages", `{"model":"test-model","stream":true,"max_tokens":100,"messages":[]}`))
			if w.Code != 200 || w.Body.String() != wire {
				t.Fatalf("wire changed: %d %s", w.Code, w.Body.String())
			}
			got := events.snapshot()
			if len(got) != 1 {
				t.Fatalf("bundles=%d want 1", len(got))
			}
			e := got[0]
			if tc.confirmed {
				if e.InputTokens != 7 || e.OutputTokens != int64(output) || e.Model != rootModel || e.NonBillable != tc.free || e.UsageStatus != usageStatusComplete || len(e.Components) != 1 {
					t.Fatalf("confirmed usage lost: %+v", e)
				}
				child := e.Components[0]
				if child.Model != childModel || child.InputTokens != 3 || child.OutputTokens != 1 || child.NonBillable || child.PricingContext == nil || child.PricingContext.TotalInputTokens != 3 {
					t.Fatalf("confirmed component lost: %+v", child)
				}
				if e.PricingContext == nil || e.PricingContext.TotalInputTokens != 7 || !e.PricingContext.TotalInputValid || hasGoneDiagnostic(e.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) || !hasGoneDiagnostic(e.UsageDiagnostic, diagnosticInterruptedAfterFinalUsage) {
					t.Fatalf("confirmed pricing context lost: %+v", e)
				}
			} else if e.InputTokens != 0 || e.OutputTokens != 0 || len(e.Components) != 0 || e.PricingContext != nil || e.UsageStatus != usageStatusIncomplete || !e.CostIncomplete || !hasGoneDiagnostic(e.UsageDiagnostic, diagnosticUpstreamInterruptedNoUsage) {
				t.Fatalf("intermediate usage incorrectly charged: %+v", e)
			}
			if e.ResponseStatus != "failed" {
				t.Fatalf("failure state lost: %+v", e)
			}
			assertCompletions(t, stats, requeststats.Failure)
			assertNoReplay(t, s, calls.Load())
			assertRejectionResources(t, s)
		})
	}
}
