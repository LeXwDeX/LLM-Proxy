package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// Issue #36: a target's ModelPrefix is added only to the outbound model. When
// the upstream echoes that outbound spelling in Claude usage, attribution must
// strip the configured prefix (only on a complete prefix match) before the
// case-insensitive comparison, and bill components/fallback roots under the
// unprefixed upstream name the request admission checked.
const prefixAdmissionBody = `{"model":"Pfx/test-model","tools":[{"type":"advisor_20260301","name":"advisor","model":"Advisor"}],"fallbacks":[{"model":"FB"}]}`

func prefixUsageParser(prefix, body string) usageParser {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	target := &Target{Name: "prefix", EndpointType: config.EndpointTypeClaude, ModelPrefix: prefix}
	return newTargetUsageParser(r, target, "test-model", []byte(body), nil)
}

type prefixIterationWant struct {
	invalid      bool
	diagnostic   string
	billingModel string
	components   []string // "type:model"
}

func prefixIterationCases() []struct {
	name, prefix, topModel, usage string
	want                          prefixIterationWant
} {
	const top = `"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0`
	msg := func(model string, in, out int) string {
		m := ""
		if model != "" {
			m = `"model":"` + model + `",`
		}
		return `{"type":"message",` + m + `"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
	}
	extra := func(typ, model string, in, out int) string {
		m := ""
		if model != "" {
			m = `"model":"` + model + `",`
		}
		return `{"type":"` + typ + `",` + m + `"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
	}
	iter := func(items ...string) string { return top + `,"iterations":[` + strings.Join(items, ",") + `]` }
	return []struct {
		name, prefix, topModel, usage string
		want                          prefixIterationWant
	}{
		// Positive: the echoed outbound spelling is stripped and admitted.
		{"advisor", "Pfx/", "Pfx/test-model", iter(msg("Pfx/test-model", 7, 2), extra("advisor_message", "Pfx/advisor", 3, 1)),
			prefixIterationWant{components: []string{"advisor_message:advisor"}}},
		{"advisor_prefix_case_insensitive", "Pfx/", "pfx/TEST-MODEL", iter(msg("pfx/TEST-MODEL", 7, 2), extra("advisor_message", "PFX/Advisor", 3, 1)),
			prefixIterationWant{components: []string{"advisor_message:advisor"}}},
		{"advisor_unprefixed_echo", "Pfx/", "test-model", iter(msg("test-model", 7, 2), extra("advisor_message", "advisor", 3, 1)),
			prefixIterationWant{components: []string{"advisor_message:advisor"}}},
		{"compaction", "Pfx/", "Pfx/test-model", iter(extra("compaction", "Pfx/test-model", 11, 2), msg("Pfx/test-model", 7, 2)),
			prefixIterationWant{components: []string{"compaction:test-model"}}},
		{"compaction_default_model", "Pfx/", "Pfx/test-model", iter(extra("compaction", "", 11, 2), msg("Pfx/test-model", 7, 2)),
			prefixIterationWant{components: []string{"compaction:test-model"}}},
		{"fallback", "Pfx/", "Pfx/fb", iter(msg("Pfx/test-model", 5, 1), extra("fallback_message", "Pfx/fb", 7, 2)),
			prefixIterationWant{billingModel: "fb", components: []string{"message:test-model"}}},
		{"message_only_mixed_echo", "Pfx/", "Pfx/test-model", top + `,"iterations":[` + msg("Pfx/test-model", 3, 1) + `,` + msg("test-model", 4, 1) + `]`,
			prefixIterationWant{}},
		// Not admitted after stripping: keep the (stripped) echo, never infer.
		{"advisor_not_admitted", "Pfx/", "Pfx/test-model", iter(msg("Pfx/test-model", 7, 2), extra("advisor_message", "Pfx/other-advisor", 3, 1)),
			prefixIterationWant{components: []string{"advisor_message:other-advisor"}}},
		// Counterexamples: no prefix target unchanged; a non-matching echo is
		// still an unverifiable plan.
		{"no_prefix_target_prefixed_echo", "", "Pfx/test-model", iter(msg("Pfx/test-model", 7, 2), extra("advisor_message", "Pfx/advisor", 3, 1)),
			prefixIterationWant{invalid: true, diagnostic: "multi_model_iterations"}},
		{"no_prefix_target_plain", "", "test-model", iter(msg("", 7, 2), extra("advisor_message", "Advisor", 3, 1)),
			prefixIterationWant{components: []string{"advisor_message:advisor"}}},
		{"other_prefix", "Pfx/", "Other/test-model", iter(msg("Other/test-model", 7, 2), extra("advisor_message", "Pfx/advisor", 3, 1)),
			prefixIterationWant{invalid: true, diagnostic: "multi_model_iterations"}},
		{"partial_prefix", "Pfx/", "Pfxtest-model", iter(msg("Pfxtest-model", 7, 2), extra("advisor_message", "Pfx/advisor", 3, 1)),
			prefixIterationWant{invalid: true, diagnostic: "multi_model_iterations"}},
		{"prefix_only", "Pfx/", "Pfx/", iter(msg("Pfx/", 7, 2), extra("advisor_message", "Pfx/advisor", 3, 1)),
			prefixIterationWant{invalid: true, diagnostic: "multi_model_iterations"}},
		{"message_only_other_prefix", "Pfx/", "Pfx/test-model", top + `,"iterations":[` + msg("Other/test-model", 7, 2) + `]`,
			prefixIterationWant{invalid: true}},
		{"fallback_message_other_prefix", "Pfx/", "Pfx/fb", iter(msg("Other/test-model", 5, 1), extra("fallback_message", "Pfx/fb", 7, 2)),
			prefixIterationWant{billingModel: "fb", components: []string{"message:other/test-model"}}},
	}
}

func checkPrefixIteration(t *testing.T, tokens usageTokens, want prefixIterationWant) {
	t.Helper()
	if tokens.invalidIterations != want.invalid {
		t.Fatalf("invalidIterations=%v want %v: %+v", tokens.invalidIterations, want.invalid, tokens)
	}
	if want.diagnostic != "" && tokens.diagnostic != want.diagnostic {
		t.Fatalf("diagnostic=%q want %q", tokens.diagnostic, want.diagnostic)
	}
	if !want.invalid && want.diagnostic == "" && (tokens.costIncomplete || tokens.diagnostic != "") {
		t.Fatalf("unexpected incomplete: %q", tokens.diagnostic)
	}
	if tokens.billingModel != want.billingModel {
		t.Fatalf("billingModel=%q want %q", tokens.billingModel, want.billingModel)
	}
	var got []string
	for _, c := range tokens.components {
		got = append(got, c.IterationType+":"+c.Model)
		if c.ResponseModel != c.Model {
			t.Fatalf("component response model %q != model %q", c.ResponseModel, c.Model)
		}
	}
	if strings.Join(got, ",") != strings.Join(want.components, ",") {
		t.Fatalf("components=%v want %v", got, want.components)
	}
}

func TestClaudeIterationsModelPrefixJSON(t *testing.T) {
	for _, tc := range prefixIterationCases() {
		t.Run(tc.name, func(t *testing.T) {
			p := prefixUsageParser(tc.prefix, prefixAdmissionBody)
			body := `{"type":"message","model":"` + tc.topModel + `","stop_reason":"end_turn","usage":{` + tc.usage + `}}`
			tokens, _, found := p.extractTokens("application/json", []byte(body))
			if !found {
				t.Fatal("usage not found")
			}
			checkPrefixIteration(t, tokens, tc.want)
		})
	}
}

func TestClaudeIterationsModelPrefixSSE(t *testing.T) {
	for _, tc := range prefixIterationCases() {
		t.Run(tc.name, func(t *testing.T) {
			p := prefixUsageParser(tc.prefix, prefixAdmissionBody)
			wire := `data: {"type":"message_start","message":{"model":"` + tc.topModel + `","usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{` + tc.usage + `}}` + "\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
			u := newUsageStream("text/event-stream", "", p)
			_, _ = io.WriteString(u, wire)
			u.Close()
			if !u.found {
				t.Fatal("usage not found")
			}
			if tc.want.invalid {
				// A first invalid final keeps the conservative legacy tokens.
				if len(u.tokens.components) != 0 || u.tokens.billingModel != "" || (tc.want.diagnostic != "" && u.tokens.diagnostic != tc.want.diagnostic) {
					t.Fatalf("invalid plan produced attribution: %+v", u.tokens)
				}
				return
			}
			checkPrefixIteration(t, u.tokens, tc.want)
		})
	}
}

// The documented fallback still names its own output when the final usage
// omits input; the prefixed echo must bill the unprefixed fallback model.
func TestClaudeFallbackMissingInputStripsModelPrefix(t *testing.T) {
	for _, tc := range []struct{ prefix, want string }{{"Pfx/", "fb"}, {"", "pfx/fb"}} {
		p := prefixUsageParser(tc.prefix, prefixAdmissionBody)
		body := `{"type":"message","model":"Pfx/fb","stop_reason":"end_turn","usage":{"output_tokens":2,"iterations":[{"type":"message","model":"Pfx/test-model","input_tokens":5,"output_tokens":1},{"type":"fallback_message","model":"Pfx/fb","input_tokens":7,"output_tokens":2}]}}`
		tokens, _, _ := p.extractTokens("application/json", []byte(body))
		if tokens.billingModel != tc.want || !strings.Contains(tokens.diagnostic, "missing_fallback_input_usage") {
			t.Fatalf("prefix=%q billingModel=%q diagnostic=%q", tc.prefix, tokens.billingModel, tokens.diagnostic)
		}
	}
}

// End to end through the Service: the wire carries the prefix, the persisted
// bundle uses unprefixed names, and an echoed supplementary model outside the
// request's admission set is logged without request or response bodies.
func TestClaudeModelPrefixIterationBundleAndWarn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, advisorEcho, wantChild string
			warn                         bool
		}{
			{"admitted", "Pfx/advisor", "advisor", false},
			{"not_admitted", "Pfx/rogue-advisor", "rogue-advisor", true},
		} {
			t.Run(tc.name+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				var wireModel string
				usageJSON := `{"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"iterations":[{"type":"message","model":"Pfx/test-model","input_tokens":7,"output_tokens":2},{"type":"advisor_message","model":"` + tc.advisorEcho + `","input_tokens":3,"output_tokens":1}]}`
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					if i := strings.Index(string(raw), `"model":"`); i >= 0 {
						rest := string(raw[i+len(`"model":"`):])
						wireModel = rest[:strings.Index(rest, `"`)]
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, `data: {"type":"message_start","message":{"model":"Pfx/test-model","usage":{"input_tokens":7,"output_tokens":0}}}`+"\n\n"+
							`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":`+usageJSON+`}`+"\n\n"+
							"data: {\"type\":\"message_stop\"}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"type":"message","model":"Pfx/test-model","stop_reason":"end_turn","usage":`+usageJSON+`}`)
				}))
				defer up.Close()
				cfg := &config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 10}, Targets: []config.Target{{
					Name: "prefix", EndpointType: config.EndpointTypeClaude, Endpoint: up.URL, APIKey: "fixture",
					ModelPrefix: "Pfx/", ModelMappings: []config.ModelMapping{{Upstream: "test-model"}},
				}}}
				logs := &rejectionLog{}
				s, err := NewService(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				events := &goneEventRecorder{}
				s.SetUsageRecorder(events)
				body := `{"model":"test-model","max_tokens":8,"stream":` + map[bool]string{false: "false", true: "true"}[stream] +
					`,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Advisor"}],"messages":[{"role":"user","content":"secret-prompt"}]}`
				w := httptest.NewRecorder()
				s.ServeHTTP(w, goneRequest(t, "/v1/messages", body))
				if w.Code != http.StatusOK {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				if wireModel != "Pfx/test-model" {
					t.Fatalf("wire model=%q", wireModel)
				}
				got := events.snapshot()
				if len(got) != 1 {
					t.Fatalf("events=%+v", got)
				}
				root := got[0]
				if root.Model != "test-model" || root.CostIncomplete || root.UsageDiagnostic != "" || len(root.Components) != 1 {
					t.Fatalf("root=%+v", root)
				}
				child := root.Components[0]
				if child.Model != tc.wantChild || child.IterationType != "advisor_message" || child.InputTokens != 3 || child.OutputTokens != 1 {
					t.Fatalf("child=%+v", child)
				}
				text := logs.String()
				warned := strings.Contains(text, "echoed supplementary model not in admission set")
				if warned != tc.warn {
					t.Fatalf("warn=%v want %v: %s", warned, tc.warn, text)
				}
				if tc.warn && (!strings.Contains(text, `"model":"rogue-advisor"`) || strings.Contains(text, "secret-prompt") || strings.Contains(text, "fixture")) {
					t.Fatalf("warn fields: %s", text)
				}
			})
		}
	}
}
