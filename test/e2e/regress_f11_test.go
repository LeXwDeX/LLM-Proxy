//go:build e2e

package e2e

import (
	"fmt"
	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRegressF11Standard(t *testing.T) { vendorStandardGuard(t, "F11") }

func TestRegressF11OfficialSuccessUsage(t *testing.T) {
	alias := uniq("e2e-f11usage")
	tg := keyedTarget(t, w.urlA, "/minimax/v1", alias, []string{uniq("sk-f11usage")}, map[string]any{"usage_adapter": "minimax"})
	for _, stream := range []bool{false, true} {
		name, key := client(t, []string{tg}, nil)
		// chatBody explicitly sets stream_options.include_usage=true for SSE.
		body := chatBody("[[fake usage.in=100 usage.out=20 usage.cached=30]]", stream)
		body["model"] = alias
		resp, wire := call(t, key, "POST", "/v1/chat/completions", body, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("HTTP %d %s", resp.StatusCode, wire)
		}
		checkBilled(t, usageFor(t, name, 1)[0], billed{in: 70, out: 20, cached: 30})
	}
}

func TestRegressF11StreamWithoutUsageOptIn(t *testing.T) {
	alias, upstreamKey := uniq("e2e-f11nousage"), uniq("sk-f11nousage")
	tg := keyedTarget(t, w.urlA, "/minimax/v1", alias, []string{upstreamKey}, map[string]any{"usage_adapter": "minimax", "request_stream_usage_if_missing": false})
	name, key := client(t, []string{tg}, nil)
	from := w.fakeA.LastSeq()
	resp, wire := call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(wire), `"usage":null`) || strings.Contains(string(wire), `"prompt_tokens"`) || !strings.Contains(string(wire), "data: [DONE]") {
		t.Fatalf("unexpected stream: HTTP %d %s", resp.StatusCode, wire)
	}
	if hits(w.fakeA, upstreamKey, from) != 1 {
		t.Fatal("stream was retried")
	}
	for _, record := range w.fakeA.RequestsSince(from) {
		if record.Key == fakellm.KeyFingerprint(upstreamKey) && record.Usage != nil {
			t.Fatal("fakellm recorded unrequested usage")
		}
	}
	ev := usageFor(t, name, 1)[0]
	if ev.ResponseStatus == "failed" || ev.UsageStatus != "incomplete" || ev.InputTokens != 0 || ev.OutputTokens != 0 {
		t.Fatalf("missing usage must not imply failed response: %+v", ev)
	}
}

func TestRegressF11AllKeysFailover(t *testing.T) {
	p, alias := pool(t), uniq("e2e-f11-failover")
	k0, k1, healthy := uniq("sk-f11-0"), uniq("sk-f11-1"), uniq("sk-f11-good")
	tg := keyedTarget(t, w.urlA, "/minimax/v1", alias, []string{k0, k1}, map[string]any{"pool": p, "usage_adapter": "minimax"})
	keyedTarget(t, w.urlB, "/minimax/v1", alias, []string{healthy}, map[string]any{"pool": p, "usage_adapter": "minimax"})
	for _, k := range []string{k0, k1} {
		w.fakeA.SetKeyScenario(k, &fakellm.Scenario{Preset: "minimax_1008"})
		defer w.fakeA.SetKeyScenario(k, nil)
	}
	for i := 0; i < 16; i++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, wire := chat(t, key, alias, "hi")
		if hits(w.fakeA, k0, fromA)+hits(w.fakeA, k1, fromA) == 0 {
			continue
		}
		if resp.StatusCode != 200 || hits(w.fakeA, k0, fromA) != 1 || hits(w.fakeA, k1, fromA) != 1 || hits(w.fakeB, healthy, fromB) != 1 {
			t.Fatalf("failover HTTP %d %s", resp.StatusCode, wire)
		}
		if !keyStatus(t, tg, 0).Exhausted || !keyStatus(t, tg, 1).Exhausted {
			t.Fatal("keys not exhausted")
		}
		return
	}
	t.Fatal("no client selected failing target")
}

func TestRegressF11Minimax(t *testing.T) {
	for _, tc := range []struct{ preset, reason string }{
		{"minimax_1008", "billing_error"}, {"minimax_1002", "rate_limited"}, {"minimax_1004", "invalid_token"}, {"minimax_2049", "invalid_token"}, {"minimax_1041", "rate_limited"}, {"minimax_2045", "rate_limited"}, {"minimax_2056", "quota_exceeded_api"}, {"minimax_2013", ""},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			k0, k1, alias := uniq("sk-f11a"), uniq("sk-f11b"), uniq("e2e-f11")
			tg := keyedTarget(t, w.urlA, "/minimax/v1", alias, []string{k0, k1}, map[string]any{"usage_adapter": "minimax"})
			name, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(k0, &fakellm.Scenario{Preset: tc.preset})
			defer w.fakeA.SetKeyScenario(k0, nil)
			from := w.fakeA.LastSeq()
			resp, wire := chat(t, key, alias, "hi")
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d %s", resp.StatusCode, wire)
			}
			if n := hits(w.fakeA, k0, from); n != 1 {
				t.Fatalf("hits=%d", n)
			}
			st := keyStatus(t, tg, 0)
			if st.Exhausted != (tc.reason != "") || st.ExhaustReason != tc.reason {
				t.Fatalf("state=%+v", st)
			}
			if tc.reason != "" {
				if hits(w.fakeA, k1, from) != 1 {
					t.Fatal("healthy key not selected")
				}
			} else {
				if hits(w.fakeA, k1, from) != 0 || string(wire) != `{"base_resp":{"status_code":2013,"status_msg":"invalid parameters"}}` {
					t.Fatalf("unknown changed: %s", wire)
				}
				ev := usageFor(t, name, 1)[0]
				if ev.ResponseStatus != "failed" {
					t.Fatalf("usage=%+v", ev)
				}
			}
		})
	}
}

func TestRegressF11AllCodesStandard(t *testing.T) {
	for _, code := range []int{1002, 1004, 1008, 1041, 2013, 2045, 2049, 2056} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			k0, k1, alias := uniq("sk-f11std0"), uniq("sk-f11std1"), uniq("e2e-f11std")
			tg := keyedTarget(t, w.urlA, "/minimax/v1", alias, []string{k0, k1}, nil)
			name, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(k0, &fakellm.Scenario{Preset: fmt.Sprintf("minimax_%d", code)})
			defer w.fakeA.SetKeyScenario(k0, nil)
			from := w.fakeA.LastSeq()
			resp, body := chat(t, key, alias, "hi")
			if resp.StatusCode != 200 || hits(w.fakeA, k0, from) != 1 || hits(w.fakeA, k1, from) != 0 || keyStatus(t, tg, 0).Exhausted {
				t.Fatalf("standard changed: HTTP %d %s", resp.StatusCode, body)
			}
			if ev := usageFor(t, name, 1)[0]; ev.ResponseStatus == "failed" {
				t.Fatalf("standard marked base_resp as failed: %+v", ev)
			}
		})
	}
}

// Adversarial bytes, not a MiniMax preset or a claimed official SSE shape.
func TestRegressF11SSEBaseRespIsNotVendorError(t *testing.T) {
	const wire = "data: {\"base_resp\":{\"status_code\":1008}}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, wire)
	}))
	defer up.Close()
	for _, adapter := range []string{"minimax", "standard"} {
		alias := uniq("e2e-f11sse")
		tg := keyedTarget(t, up.URL, "/v1", alias, []string{uniq("sk-sse0"), uniq("sk-sse1")}, map[string]any{"usage_adapter": adapter})
		name, key := client(t, []string{tg}, nil)
		before := calls.Load()
		resp, body := call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
		if resp.StatusCode != 200 || string(body) != wire || calls.Load()-before != 1 || keyStatus(t, tg, 0).Exhausted {
			t.Fatalf("%s: SSE changed HTTP %d %s", adapter, resp.StatusCode, body)
		}
		ev := usageFor(t, name, 1)[0]
		if ev.ResponseStatus == "failed" || ev.InputTokens != 7 || ev.OutputTokens != 3 {
			t.Fatalf("%s: usage=%+v", adapter, ev)
		}
	}
}

func TestRegressF11Non2xxUsesOriginalProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, path, prefix, endpoint, preset, body string
		status, badCalls, goodCalls                int
	}{
		{"chat429", "/v1/chat/completions", "/minimax/v1", "openai", "", `{"base_resp":{"status_code":1008}}`, 429, 2, 1},
		{"chat401", "/v1/chat/completions", "/minimax/v1", "openai", "", `{"base_resp":{"status_code":1004}}`, 401, 1, 0},
		{"messages429", "/v1/messages", "/minimax/anthropic", "claude", "rate_limit", "", 429, 2, 1},
		{"messages529", "/v1/messages", "/minimax/anthropic", "claude", "overloaded", "", 529, 1, 0},
	} {
		for _, adapter := range []string{"minimax", "standard"} {
			t.Run(tc.name+"/"+adapter, func(t *testing.T) {
				k0, k1, alias := uniq("sk-f11http0"), uniq("sk-f11http1"), uniq("e2e-f11http")
				tg := keyedTarget(t, w.urlA, tc.prefix, alias, []string{k0, k1}, map[string]any{"usage_adapter": adapter, "endpoint_type": tc.endpoint})
				_, key := client(t, []string{tg}, nil)
				w.fakeA.SetKeyScenario(k0, &fakellm.Scenario{Status: tc.status, Preset: tc.preset, Body: tc.body})
				defer w.fakeA.SetKeyScenario(k0, nil)
				from := w.fakeA.LastSeq()
				resp, body := call(t, key, "POST", tc.path, map[string]any{"model": alias, "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
				want := tc.status
				if tc.goodCalls > 0 {
					want = 200
				}
				if resp.StatusCode != want || hits(w.fakeA, k0, from) != tc.badCalls || hits(w.fakeA, k1, from) != tc.goodCalls {
					t.Fatalf("HTTP %d, hits=%d/%d: %s", resp.StatusCode, hits(w.fakeA, k0, from), hits(w.fakeA, k1, from), body)
				}
				st := keyStatus(t, tg, 0)
				if st.Exhausted != (tc.status == 429) || (st.Exhausted && st.ExhaustReason != "rate_limited") {
					t.Fatalf("wrong key state: %+v", st)
				}
			})
		}
	}
}
