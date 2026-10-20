//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// proxyRetries reads the proxy's global key + target retry counter.
func proxyRetries(t *testing.T) int64 {
	t.Helper()
	code, body := w.adminDo("GET", "/admin/metrics", nil)
	if code != 200 {
		t.Fatalf("metrics: %d %s", code, body)
	}
	var m struct {
		Requests struct {
			Retries int64 `json:"retries"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("metrics decode: %v", err)
	}
	return m.Requests.Retries
}

// TestRegressDeepSeekAbortedFinish: DeepSeek documents Chat finish_reason
// "aborted" ("生成过程被中断",
// https://api-docs.deepseek.com/zh-cn/api/create-chat-completion, checked
// 2026-10-20; fakellm preset deepseek_aborted is the documentation contract,
// not live acceptance). With the deepseek Patch selected the request lifecycle
// is failed (usage event response_status=failed); with standard
// (counterexample) it is not. Both keep the original reason and bill the exact
// reported usage, and neither retries, switches keys nor fails over. The
// request-statistics outcome is asserted in the proxy unit test because the
// persisted request statistics only expose completed minutes.
func TestRegressDeepSeekAbortedFinish(t *testing.T) {
	for _, adapter := range []string{"deepseek", "standard"} {
		for _, stream := range []bool{false, true} {
			name := adapter + "/json"
			if stream {
				name = adapter + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				alias := uniq("e2e-ds-aborted")
				tg := target(t, map[string]any{"endpoint_type": "dual_protocol", "usage_adapter": adapter, "endpoint": w.urlA + "/deepseek",
					"anthropic_prefix": "/anthropic", "api_key": uniq("sk-ds-aborted"), "api_keys": []string{uniq("sk-ds-aborted-b")}, "model_mappings": []any{map[string]any{"upstream": "deepseek-chat", "fallback": alias}}})
				clientName, key := client(t, []string{tg}, nil)
				body := map[string]any{"model": alias, "stream": stream,
					"messages": []any{map[string]any{"role": "user", "content": "[[fake preset=deepseek_aborted usage.in=100 usage.out=7 usage.cached=40]]"}}}
				retries := proxyRetries(t)
				before := w.fakeA.LastSeq()
				resp, raw := call(t, key, "POST", "/v1/chat/completions", body, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("HTTP %d %s", resp.StatusCode, raw)
				}
				if records := w.fakeA.RequestsSince(before); len(records) != 1 || records[0].Outcome != "ok" {
					t.Fatalf("upstream calls=%d, want exactly one: %+v", len(records), records)
				}
				ev := usageFor(t, clientName, 1)[0]
				wantStatus := ""
				if adapter == "deepseek" {
					wantStatus = "failed"
				}
				if ev.ResponseStatus != wantStatus || ev.FinishReason != "aborted" || !strings.HasPrefix(ev.UsageAdapter, adapter) {
					t.Fatalf("response_status=%q (want %q) finish_reason=%q adapter=%q", ev.ResponseStatus, wantStatus, ev.FinishReason, ev.UsageAdapter)
				}
				if ev.UsageStatus != "complete" || ev.CostIncomplete || ev.UsageDiagnostic != "" || ev.InputTokens != 60 || ev.CachedTokens != 40 || ev.OutputTokens != 7 {
					t.Fatalf("usage not billed exactly on aborted finish: %+v", ev)
				}
				if r := proxyRetries(t); r != retries {
					t.Fatalf("aborted finish triggered retries: +%d", r-retries)
				}
				for i := 0; i < 2; i++ {
					if st := keyStatus(t, tg, i); st.Exhausted || st.ExhaustReason != "" || !st.CooldownEnd.IsZero() {
						t.Fatalf("aborted finish disabled or cooled key %d: %+v", i, st)
					}
				}
			})
		}
	}
}
