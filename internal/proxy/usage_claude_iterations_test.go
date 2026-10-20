package proxy

import (
	"fmt"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Claude usage.iterations contract (issue #7). Shapes follow the official
// advisor tool, threshold/on-demand compaction and fallback docs checked
// 2026-10-21; a documentation check is not acceptance against the real API.
// Top-level counters are always billed as-is; iterations only decide whether
// the cost is complete and which diagnostic explains it.
func TestClaudeUsageIterations(t *testing.T) {
	const advisorItem = `{"type":"advisor_message","model":"claude-opus-5-5","input_tokens":823,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1612}`
	const execA = `{"type":"message","input_tokens":412,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":89}`
	const execB = `{"type":"message","input_tokens":1348,"cache_read_input_tokens":412,"cache_creation_input_tokens":0,"output_tokens":442}`
	const advisorTop = `"input_tokens":1760,"cache_read_input_tokens":412,"cache_creation_input_tokens":0,"output_tokens":531`
	for _, tc := range []struct {
		name, usage   string
		in, out, read int64
		incomplete    bool
		diagnostic    string
	}{
		{"absent", `"input_tokens":70,"output_tokens":10`, 70, 10, 0, false, ""},
		{"empty_array", `"input_tokens":70,"output_tokens":10,"iterations":[]`, 70, 10, 0, false, ""},
		{"null", `"input_tokens":70,"output_tokens":10,"iterations":null`, 70, 10, 0, false, ""},
		{"non_array", `"input_tokens":70,"output_tokens":10,"iterations":{"type":"message"}`, 70, 10, 0, false, ""},
		// Threshold compaction doc: items may omit cache counters entirely.
		{"single_message_without_cache_fields", `"input_tokens":70,"output_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":0,"iterations":[{"type":"message","input_tokens":70,"output_tokens":10}]`, 70, 10, 30, false, ""},
		{"single_message_all_fields", `"input_tokens":21,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"type":"message","input_tokens":21,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]`, 21, 4, 0, false, ""},
		// Advisor doc example without its advisor item: top = sum of executors.
		{"multiple_messages_sum", advisorTop + `,"iterations":[` + execA + `,` + execB + `]`, 1760, 531, 412, false, ""},
		{"same_model_messages", `"input_tokens":30,"output_tokens":6,"iterations":[{"type":"message","model":"claude-x","input_tokens":10,"output_tokens":2},{"type":"message","model":"claude-x","input_tokens":20,"output_tokens":4}]`, 30, 6, 0, false, ""},
		{"output_mismatch", `"input_tokens":1760,"cache_read_input_tokens":412,"cache_creation_input_tokens":0,"output_tokens":530,"iterations":[` + execA + `,` + execB + `]`, 1760, 530, 412, true, "iterations_usage_mismatch"},
		{"cache_read_mismatch", `"input_tokens":1760,"cache_read_input_tokens":400,"cache_creation_input_tokens":0,"output_tokens":531,"iterations":[` + execA + `,` + execB + `]`, 1760, 531, 400, true, "iterations_usage_mismatch"},
		{"cache_creation_mismatch", `"input_tokens":70,"output_tokens":10,"cache_creation_input_tokens":0,"iterations":[{"type":"message","input_tokens":70,"output_tokens":10,"cache_creation_input_tokens":5}]`, 70, 10, 0, true, "iterations_usage_mismatch"},
		// A counter not reported by every message item is not compared.
		{"partially_reported_field_skipped", `"input_tokens":30,"output_tokens":6,"cache_read_input_tokens":99,"iterations":[{"type":"message","input_tokens":10,"output_tokens":2,"cache_read_input_tokens":1},{"type":"message","input_tokens":20,"output_tokens":4}]`, 30, 6, 99, false, ""},
		{"advisor_doc_example", advisorTop + `,"iterations":[` + execA + `,` + advisorItem + `,` + execB + `]`, 1760, 531, 412, true, "multi_model_iterations"},
		{"fallback_message", `"input_tokens":10,"output_tokens":2,"iterations":[{"type":"message","input_tokens":5,"output_tokens":0},{"type":"fallback_message","model":"claude-fallback","input_tokens":10,"output_tokens":2}]`, 10, 2, 0, true, "multi_model_iterations"},
		{"message_models_differ", `"input_tokens":30,"output_tokens":6,"iterations":[{"type":"message","model":"claude-a","input_tokens":10,"output_tokens":2},{"type":"message","model":"claude-b","input_tokens":20,"output_tokens":4}]`, 30, 6, 0, true, "multi_model_iterations"},
		// Threshold compaction doc example.
		{"compaction_and_message", `"input_tokens":23000,"output_tokens":1000,"iterations":[{"type":"compaction","input_tokens":180000,"output_tokens":3500},{"type":"message","input_tokens":23000,"output_tokens":1000}]`, 23000, 1000, 0, true, "compaction_iterations"},
		// On-demand compaction: top-level zero, usage only in the compaction item.
		{"on_demand_compaction_only", `"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction","input_tokens":180000,"output_tokens":3500}]`, 0, 0, 0, true, "compaction_iterations"},
		// Issue #7 reduced fixture: the item has no type, so attribution is
		// unknown and stays conservative on purpose. Whether a real response
		// carries type is left to an authorized live readback.
		{"issue7_item_without_type", `"input_tokens":21,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"input_tokens":21,"output_tokens":4,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]`, 21, 4, 0, true, "unknown_usage_iterations"},
		{"unknown_type", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"future_message","input_tokens":70,"output_tokens":10}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"non_string_type", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":1,"input_tokens":70,"output_tokens":10}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"non_object_item", `"input_tokens":70,"output_tokens":10,"iterations":["message"]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"null_item", `"input_tokens":70,"output_tokens":10,"iterations":[null]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"negative_count", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","input_tokens":-1,"output_tokens":10}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"fraction_count", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","input_tokens":70,"output_tokens":1.5}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"string_count", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","input_tokens":"70","output_tokens":10}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"overflow_count", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","input_tokens":9223372036854775808,"output_tokens":10}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		{"sum_overflow", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","input_tokens":9223372036854775807,"output_tokens":5},{"type":"message","input_tokens":9223372036854775807,"output_tokens":5}]`, 70, 10, 0, true, "iterations_usage_mismatch"},
		{"non_string_model", `"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","model":7,"input_tokens":70,"output_tokens":10}]`, 70, 10, 0, true, "unknown_usage_iterations"},
		// Priority: multi_model > compaction > unknown > mismatch.
		{"priority_multi_model_over_compaction", `"input_tokens":10,"output_tokens":2,"iterations":[{"type":"compaction","input_tokens":5,"output_tokens":1},` + advisorItem + `,{"type":"message","input_tokens":10,"output_tokens":2}]`, 10, 2, 0, true, "multi_model_iterations"},
		{"priority_multi_model_over_unknown", `"input_tokens":10,"output_tokens":2,"iterations":[{"input_tokens":1,"output_tokens":1},` + advisorItem + `]`, 10, 2, 0, true, "multi_model_iterations"},
		{"priority_compaction_over_unknown", `"input_tokens":10,"output_tokens":2,"iterations":["x",{"type":"compaction","input_tokens":5,"output_tokens":1}]`, 10, 2, 0, true, "compaction_iterations"},
		{"priority_unknown_over_mismatch", `"input_tokens":10,"output_tokens":2,"iterations":[{"type":"message","input_tokens":9,"output_tokens":2},{"type":"other","input_tokens":1,"output_tokens":0}]`, 10, 2, 0, true, "unknown_usage_iterations"},
		// Existing server tool rule still overrides the iterations diagnostic.
		{"server_tool_charges_over_iterations", advisorTop + `,"server_tool_use":{"web_search_requests":1},"iterations":[` + execA + `,` + advisorItem + `,` + execB + `]`, 1760, 531, 412, true, "server_tool_charges"},
		{"server_tool_zero_with_message", `"input_tokens":70,"output_tokens":10,"server_tool_use":{"web_fetch_requests":0,"web_search_requests":0},"iterations":[{"type":"message","input_tokens":70,"output_tokens":10}]`, 70, 10, 0, false, ""},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				body := `{"usage":{` + tc.usage + `}}`
				p := usageParser{protocol: config.ProtocolAnthropicMessages}
				var got usageTokens
				var found bool
				if stream {
					// message_start carries preliminary counters without
					// iterations; message_delta carries the cumulative usage
					// with the updated iterations array (advisor doc).
					wire := `data: {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
						`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + tc.usage + `}}` + "\n\n" +
						`data: {"type":"message_stop"}` + "\n\n"
					got, _, found = p.extractSSE([]byte(wire))
				} else {
					got, _, found = p.extractTokens("application/json", []byte(body))
				}
				if !found || got.status != usageStatusComplete {
					t.Fatalf("found=%v got %+v", found, got)
				}
				if got.InputTokens != tc.in || got.OutputTokens != tc.out || got.CachedTokens != tc.read {
					t.Fatalf("top-level counters changed: %+v", got)
				}
				if got.costIncomplete != tc.incomplete || got.diagnostic != tc.diagnostic {
					t.Fatalf("cost_incomplete=%v diagnostic=%q, want %v %q", got.costIncomplete, got.diagnostic, tc.incomplete, tc.diagnostic)
				}
			})
		}
	}
}

// Streams may also put iterations on message_start; the message_delta array
// replaces it as a whole rather than merging stale items.
func TestClaudeUsageIterationsStreamReplacesStartArray(t *testing.T) {
	wire := `data: {"type":"message_start","message":{"usage":{"input_tokens":70,"output_tokens":1,"iterations":[{"type":"message","input_tokens":70,"output_tokens":1},{"type":"message","input_tokens":0,"output_tokens":0}]}}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":70,"output_tokens":10,"iterations":[{"type":"message","input_tokens":70,"output_tokens":10}]}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	got, _, found := (usageParser{protocol: config.ProtocolAnthropicMessages}).extractSSE([]byte(wire))
	if !found || got.status != usageStatusComplete || got.InputTokens != 70 || got.OutputTokens != 10 || got.costIncomplete || got.diagnostic != "" {
		t.Fatalf("found=%v got %+v", found, got)
	}
}
