//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestSupplementaryAdmission(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"advisor", `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"Admission-Child"}]`},
		{"fallback", `,"fallback_model":"Admission-Child"`},
		{"official_first", `,"fallbacks":[{"model":"Admission-Child"},{"model":"Admission-Other"}]`},
		{"official_later", `,"fallbacks":[{"model":"Admission-Other"},{"model":"Admission-Child"}]`},
		{"official_first_duplicates", `,"fallbacks":[{"model":"Admission-Other","model":"Admission-Child","Model":"Admission-Other"}],"Fallbacks":[]`},
		{"official_later_duplicates", `,"fallbacks":[],"fallbacks":[{"model":"Admission-Other"},{"Model":"Admission-Other","model":"Admission-Child"}],"Fallbacks":[]`},
	} {
		for _, coefficient := range []float64{1, 0} {
			t.Run(fmt.Sprintf("%s/coefficient=%g", tc.name, coefficient), func(t *testing.T) {
				alias, childAlias := uniq("executor"), uniq("child")
				tg := target(t, map[string]any{"endpoint_type": "claude", "endpoint": w.urlA + "/anthropic", "api_key": uniq("sk-admission"),
					"model_mappings": []any{map[string]any{"upstream": "Admission-Executor", "fallback": alias}, map[string]any{"upstream": "Admission-Child", "fallback": childAlias}}})
				iterationPrice(t, tg, "Admission-Executor", usage.CostRates{InputPer1MTokens: 1, OutputPer1MTokens: 1})
				iterationPrice(t, tg, "Admission-Child", usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 10})
				level := uniq("admission-level")
				if code, raw := w.adminDo("POST", "/admin/data/job-levels", map[string]any{"name": level, "coefficient_daily": 1}); code != 201 {
					t.Fatalf("level HTTP %d: %s", code, raw)
				}
				t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/job-levels/"+level, nil) })
				groups := []string{uniq("executor-group"), uniq("child-group")}
				for i, model := range []string{"Admission-Executor", "Admission-Child"} {
					factor := 1.0
					if i == 1 {
						factor = coefficient
					}
					group := groups[i]
					if code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "daily_usd": 1, "cost_coefficient": factor,
						"bindings": []any{map[string]any{"target": tg, "model": model}}}); code != 201 {
						t.Fatalf("group HTTP %d: %s", code, raw)
					}
					t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
				}
				name, key := client(t, []string{tg}, nil)
				if code, raw := w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": level,
					"quota_groups": []any{map[string]any{"name": groups[0]}, map[string]any{"name": groups[1]}}}); code != 200 {
					t.Fatalf("plan HTTP %d: %s", code, raw)
				}
				fields := tc.fields
				body := fmt.Sprintf(`{"model":%q,"max_tokens":32,"messages":[{"role":"user","content":"[[fake pricing=standard usage.in=10 usage.out=2 usage.cached=0 usage.cw=0 usage.reasoning=0]]"}]%s}`, alias, fields)
				// Both groups start below their limits.
				response, raw := call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("initial request HTTP %d: %s", response.StatusCode, raw)
				}
				// Only the child's group is exhausted by this independent request.
				response, raw = call(t, key, "POST", "/v1/messages", expand(messagesBody("[[fake pricing=standard usage.in=1000000 usage.out=0 usage.cached=0 usage.cw=0 usage.reasoning=0]]", false), childAlias), nil)
				if response.StatusCode != 200 {
					t.Fatalf("seed HTTP %d: %s", response.StatusCode, raw)
				}
				usageFor(t, name, 2)
				code, quotaRaw := w.adminDo("GET", "/admin/data/quota/"+name, nil)
				state := decode(t, quotaRaw)
				if code != 200 || dig(state, "groups/"+groups[1]+"/quotas/daily/used") != 10*coefficient {
					t.Fatalf("child quota: HTTP %d %s", code, quotaRaw)
				}
				for _, stream := range []bool{false, true} {
					seq := w.fakeA.LastSeq()
					req := body[:len(body)-1] + fmt.Sprintf(`,"stream":%t}`, stream)
					response, raw = call(t, key, "POST", "/v1/messages", req, nil)
					if coefficient == 0 {
						if response.StatusCode != 200 || w.fakeA.LastSeq() == seq {
							t.Fatalf("free group request HTTP %d: %s", response.StatusCode, raw)
						}
						continue
					}
					if response.StatusCode != 429 || dig(decode(t, raw), "error/quota/group_name") != groups[1] || dig(decode(t, raw), "error/code") != "quota_exceeded" {
						t.Errorf("exhausted child request HTTP %d: %s", response.StatusCode, raw)
					}
					if records := w.fakeA.RequestsSince(seq); len(records) != 0 {
						t.Errorf("rejected request reached fakellm: %+v", records)
					}
				}
				wantEvents := 2
				if coefficient == 0 {
					wantEvents = 4
				}
				// Operator injection and replacement must use the final body too.
				custom := decode(t, []byte("{"+fields[1:]+"}"))
				if code, raw := w.adminDo("PUT", "/admin/data/targets/"+tg, map[string]any{"custom_body": custom}); code != 200 {
					t.Fatalf("inject custom body HTTP %d: %s", code, raw)
				}
				seq := w.fakeA.LastSeq()
				response, raw = call(t, key, "POST", "/v1/messages", strings.Replace(body, fields, "", 1), nil)
				wantStatus := 429
				if coefficient == 0 {
					wantStatus = 200
					wantEvents++
				}
				if response.StatusCode != wantStatus || (coefficient != 0 && w.fakeA.LastSeq() != seq) {
					t.Fatalf("injected supplementary model HTTP %d: %s", response.StatusCode, raw)
				}
				for key := range custom {
					custom[key] = nil
				}
				if strings.HasPrefix(tc.name, "official_") {
					custom["fallbacks"] = []any{map[string]any{"model": "Admission-Other"}}
				}
				if code, raw := w.adminDo("PUT", "/admin/data/targets/"+tg, map[string]any{"custom_body": custom}); code != 200 {
					t.Fatalf("remove custom field HTTP %d: %s", code, raw)
				}
				response, raw = call(t, key, "POST", "/v1/messages", body, nil)
				if response.StatusCode != 200 {
					t.Fatalf("removed supplementary model HTTP %d: %s", response.StatusCode, raw)
				}
				wantEvents++
				if strings.HasPrefix(tc.name, "official_") {
					if code, raw := w.adminDo("PUT", "/admin/data/targets/"+tg, map[string]any{"custom_body": map[string]any{"fallbacks": "default"}}); code != 200 {
						t.Fatalf("default fallback HTTP %d: %s", code, raw)
					}
					response, raw = call(t, key, "POST", "/v1/messages", body, nil)
					if response.StatusCode != 200 {
						t.Fatalf("default routing must not infer a model: HTTP %d %s", response.StatusCode, raw)
					}
					wantEvents++
				}
				events := usageFor(t, name, wantEvents)
				if len(events) != wantEvents {
					t.Fatalf("usage events=%d want=%d; rejected requests must not bill", len(events), wantEvents)
				}
				for _, event := range events {
					if event.UsageStatus != usage.UsageStatusComplete || event.CostIncomplete || event.UsageDiagnostic != "" {
						t.Fatalf("unexpected usage diagnostic: %+v", event)
					}
				}
			})
		}
	}
}
