//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// hits counts upstream requests made with key after the journal cursor from
// (fakellm.Server.LastSeq).
func hits(fake *fakellm.Server, key string, from int64) int {
	fp, n := fakellm.KeyFingerprint(key), 0
	for _, r := range fake.RequestsSince(from) {
		if r.Key == fp {
			n++
		}
	}
	return n
}

func chat(t *testing.T, key, alias, content string) (*http.Response, []byte) {
	t.Helper()
	return call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias,
		"messages": []any{map[string]any{"role": "user", "content": content}}}, nil)
}

func keyedTarget(t *testing.T, fakeURL, prefix, alias string, keys []string, extra map[string]any) string {
	spec := map[string]any{"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": fakeURL + prefix,
		"api_key": keys[0], "model_mappings": []any{map[string]any{"upstream": "up-" + alias, "fallback": alias}}}
	if len(keys) > 1 {
		spec["api_keys"] = keys[1:]
	}
	for k, v := range extra {
		spec[k] = v
	}
	return target(t, spec)
}

// Key pool: a rate-limited key is retried briefly, then the pool moves on.
func TestKeyPoolRateLimitRotates(t *testing.T) {
	k1, k2 := uniq("sk-rl1"), uniq("sk-rl2")
	tg := keyedTarget(t, w.urlA, "/openai/v1", "e2e-rl", []string{k1, k2}, nil)
	_, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(k1, &fakellm.Scenario{Preset: "rate_limit", RetryAfter: "1"})
	defer w.fakeA.SetKeyScenario(k1, nil)
	from := w.fakeA.LastSeq()
	for i := 0; i < 3; i++ {
		if resp, body := chat(t, key, "e2e-rl", "hi"); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if hits(w.fakeA, k2, from) < 3 || hits(w.fakeA, k1, from) == 0 {
		t.Fatalf("k1=%d k2=%d upstream hits", hits(w.fakeA, k1, from), hits(w.fakeA, k2, from))
	}
}

// Key pool: billing exhaustion (402) disables the key at once.
func TestKeyPoolBillingExhaustionDisablesKey(t *testing.T) {
	for _, preset := range []string{"billing", "deepseek_402", "credit_exhausted", "invalid_key"} {
		t.Run(preset, func(t *testing.T) {
			k1, k2 := uniq("sk-bill1"), uniq("sk-bill2")
			alias := uniq("e2e-bill")
			tg := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{k1, k2}, nil)
			_, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(k1, &fakellm.Scenario{Preset: preset})
			defer w.fakeA.SetKeyScenario(k1, nil)
			from := w.fakeA.LastSeq()
			for i := 0; i < 4; i++ {
				if resp, body := chat(t, key, alias, "hi"); resp.StatusCode != 200 {
					t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
				}
			}
			// 402/401 and a 429-status exhaustion code (#159 F3) all disable
			// the key at once: exactly one upstream hit.
			if n := hits(w.fakeA, k1, from); n != 1 {
				t.Fatalf("exhausted key used %d times, want 1", n)
			}
		})
	}
}

// Account-level failure on every key is passed through, not masked.
func TestKeyPoolAllKeysExhaustedPassesThrough(t *testing.T) {
	k1, k2 := uniq("sk-dead1"), uniq("sk-dead2")
	tg := keyedTarget(t, w.urlA, "/openai/v1", "e2e-dead", []string{k1, k2}, nil)
	_, key := client(t, []string{tg}, nil)
	for _, k := range []string{k1, k2} {
		w.fakeA.SetKeyScenario(k, &fakellm.Scenario{Preset: "billing"})
		defer w.fakeA.SetKeyScenario(k, nil)
	}
	resp, body := chat(t, key, "e2e-dead", "hi")
	if resp.StatusCode < 400 {
		t.Fatalf("all keys exhausted but got %d %s", resp.StatusCode, body)
	}
}

// Pool failover follows the red lines in internal/proxy/AGENTS.md:
//   - ordinary upstream 5xx is passed through (never replayed, the upstream
//     may have computed) and the target is muted, so later requests avoid it;
//   - retryable transport errors fail over within the same request;
//   - plain 4xx is the client's problem: passed through, target not muted.
func TestPoolFailover(t *testing.T) {
	cases := []struct {
		name string
		bad  fakellm.Scenario
		mode string // mute, retry, passthrough
	}{
		{"server-500", fakellm.Scenario{Status: 500}, "mute"},
		{"overloaded-529", fakellm.Scenario{Preset: "overloaded"}, "mute"},
		{"unavailable-503", fakellm.Scenario{Preset: "deepseek_503"}, "mute"},
		{"transport-abort", fakellm.Scenario{Abort: true}, "retry"},
		{"bad-request-400", fakellm.Scenario{Preset: "bad_request"}, "passthrough"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pool(t)
			alias := uniq("e2e-fo")
			kBad, kGood := uniq("sk-bad"), uniq("sk-good")
			keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kBad}, map[string]any{"pool": p})
			keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kGood}, map[string]any{"pool": p})
			sc := tc.bad
			w.fakeA.SetKeyScenario(kBad, &sc)
			defer w.fakeA.SetKeyScenario(kBad, nil)
			// Pool selection is sticky per client (affinity) and random on
			// first use: take fresh clients until one is first routed to the
			// bad target, then observe what that client sees afterwards.
			var key string
			failedBefore := 0
			for attempt := 0; attempt < 16; attempt++ {
				_, k := client(t, nil, []string{p})
				from := w.fakeA.LastSeq()
				resp, _ := chat(t, k, alias, "hi")
				if hits(w.fakeA, kBad, from) > 0 {
					key = k
					if resp.StatusCode != 200 {
						failedBefore = 1
					}
					break
				}
			}
			if key == "" {
				t.Fatalf("no fresh client was routed to the bad target first in 16 attempts")
			}
			after := w.fakeA.LastSeq()
			failedAfter := 0
			for i := 0; i < 6; i++ {
				resp, body := chat(t, key, alias, "hi")
				if resp.StatusCode != 200 {
					failedAfter++
					if tc.mode == "passthrough" && !strings.Contains(string(body), "fake invalid request") {
						t.Fatalf("400 body not passed through: %s", body)
					}
				}
			}
			badAfter := hits(w.fakeA, kBad, after)
			switch tc.mode {
			case "mute":
				// The first 5xx reaches the client once; the target is then muted.
				if failedBefore != 1 || badAfter != 0 || failedAfter != 0 {
					t.Fatalf("first-hit failures=%d, later bad hits=%d, later failures=%d; want 1/0/0", failedBefore, badAfter, failedAfter)
				}
			case "retry":
				if failedBefore+failedAfter != 0 {
					t.Fatalf("%d client failures; transport errors must fail over", failedBefore+failedAfter)
				}
			case "passthrough":
				if failedBefore != 1 || failedAfter != badAfter {
					t.Fatalf("first-hit failures=%d, later failures=%d vs bad hits=%d: 4xx must pass through, unmuted", failedBefore, failedAfter, badAfter)
				}
			}
		})
	}
}

// Cancellation and post-send timeouts must never be retried: the upstream
// may already be generating (and billing).
func TestCancelIsNotRetried(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-cancel")
	kA, kB := uniq("sk-slowA"), uniq("sk-slowB")
	keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	_, key := client(t, nil, []string{p})
	slow := &fakellm.Scenario{DelayMS: 1500}
	w.fakeA.SetKeyScenario(kA, slow)
	w.fakeB.SetKeyScenario(kB, slow)
	defer w.fakeA.SetKeyScenario(kA, nil)
	defer w.fakeB.SetKeyScenario(kB, nil)
	fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", w.base+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+alias+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	time.Sleep(2500 * time.Millisecond)
	total := hits(w.fakeA, kA, fromA) + hits(w.fakeB, kB, fromB)
	// The fake records a request when it finishes or notices cancellation.
	if total != 1 {
		t.Fatalf("canceled request reached upstream %d times, want 1", total)
	}
}

// A stream that dies mid-generation is not replayed elsewhere.
func TestMidStreamCutIsNotRetried(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-cut")
	kA, kB := uniq("sk-cutA"), uniq("sk-cutB")
	keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	_, key := client(t, nil, []string{p})
	cut := &fakellm.Scenario{CutAfter: 2}
	w.fakeA.SetKeyScenario(kA, cut)
	w.fakeB.SetKeyScenario(kB, cut)
	defer w.fakeA.SetKeyScenario(kA, nil)
	defer w.fakeB.SetKeyScenario(kB, nil)
	fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
	req, _ := http.NewRequest("POST", w.base+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+alias+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		b := make([]byte, 1<<16)
		for {
			if _, err := resp.Body.Read(b); err != nil {
				break
			}
		}
		resp.Body.Close()
	}
	if n := hits(w.fakeA, kA, fromA) + hits(w.fakeB, kB, fromB); n != 1 {
		t.Fatalf("cut stream reached upstream %d times, want 1", n)
	}
}

// GLM: business-code 429s cool the key down; the historical HTTP-200 SSE
// rate limit on the Anthropic endpoint is detected too.
func TestGLMRateLimits(t *testing.T) {
	cases := []struct {
		name   string
		target map[string]any
		path   string
		body   func(alias string) map[string]any
		bad    fakellm.Scenario
	}{
		{"paas-1308", map[string]any{"endpoint_type": "openai", "usage_adapter": "glm", "endpoint": w.urlA + "/glm/api/paas/v4"},
			"/v1/chat/completions", func(a string) map[string]any {
				return map[string]any{"model": a, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			}, fakellm.Scenario{Preset: "glm_1308"}},
		{"paas-1302", map[string]any{"endpoint_type": "openai", "usage_adapter": "glm", "endpoint": w.urlA + "/glm/api/paas/v4"},
			"/v1/chat/completions", func(a string) map[string]any {
				return map[string]any{"model": a, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			}, fakellm.Scenario{Preset: "glm_1302"}},
		{"anthropic-200-sse", map[string]any{"endpoint_type": "claude", "usage_adapter": "glm", "endpoint": w.urlA + "/glm/api/anthropic"},
			"/v1/messages", func(a string) map[string]any {
				return map[string]any{"model": a, "max_tokens": 16, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			}, fakellm.Scenario{SSEError: true, Preset: "glm_1308"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alias := uniq("e2e-glm")
			k1, k2 := uniq("sk-glm1"), uniq("sk-glm2")
			spec := tc.target
			spec["api_key"], spec["api_keys"] = k1, []string{k2}
			spec["model_mappings"] = []any{map[string]any{"upstream": "glm-4.6", "fallback": alias}}
			tg := target(t, spec)
			_, key := client(t, []string{tg}, nil)
			bad := tc.bad
			w.fakeA.SetKeyScenario(k1, &bad)
			defer w.fakeA.SetKeyScenario(k1, nil)
			from := w.fakeA.LastSeq()
			for i := 0; i < 3; i++ {
				resp, body := call(t, key, "POST", tc.path, tc.body(alias), nil)
				if resp.StatusCode != 200 || strings.Contains(string(body), "rate_limit_error") {
					t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
				}
			}
			if n := hits(w.fakeA, k1, from); n < 1 || n > 2 {
				t.Fatalf("rate-limited GLM key used %d times, want cooled down after 1-2", n)
			}
		})
	}
}
