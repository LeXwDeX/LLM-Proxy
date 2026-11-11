//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// targetRuntime returns the /admin/metrics targets_runtime entry of name.
func targetRuntime(t *testing.T, name string) map[string]any {
	t.Helper()
	list, _ := readMetrics(t)["targets_runtime"].([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && strings.EqualFold(m["name"].(string), name) {
			return m
		}
	}
	t.Fatalf("targets_runtime has no %s", name)
	return nil
}

func assertRejectionCooldown(t *testing.T, name string, status int) {
	t.Helper()
	rt := targetRuntime(t, name)
	if rt["phase"] != "muted" || rt["mute_reason"] != "upstream_"+itoa(status) || int(rt["mute_status"].(float64)) != status {
		t.Fatalf("%s runtime = %v, want %d cooldown", name, rt, status)
	}
	until, err := time.Parse(time.RFC3339, rt["muted_until"].(string))
	if err != nil {
		t.Fatalf("muted_until: %v", err)
	}
	if remaining := time.Until(until); remaining < 58*time.Minute || remaining > time.Hour+time.Minute {
		t.Fatalf("%s cooldown %v, want ~1h", name, remaining)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// rejectThenServe routes fresh clients until one reaches the rejecting
// candidate A first, and checks that the request is served by B.
func rejectThenServe(t *testing.T, p, alias, kA, kB string) {
	t.Helper()
	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("client saw %d %s; want 200 after account-rejection failover", resp.StatusCode, body)
		}
		if hits(w.fakeB, kB, fromB) == 0 {
			t.Fatalf("candidate B was not called after A rejected the account")
		}
		return
	}
	t.Fatalf("no fresh client was routed to the rejecting target first")
}

// An upstream 403 moves the request to the next candidate and cools the
// target for an hour: later requests never reach it.
func TestPool403FailsOverAndCoolsE2E(t *testing.T) {
	before := metricInt(t, "requests/upstream_rejection_muted_total")
	p := pool(t)
	alias := uniq("e2e-403")
	kA, kB := uniq("sk-403-a"), uniq("sk-403-b")
	tgA := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusForbidden, Body: `{"error":{"type":"permission_error","code":"account_suspended","message":"suspended"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	rejectThenServe(t, p, alias, kA, kB)
	assertRejectionCooldown(t, tgA, http.StatusForbidden)
	if rt := targetRuntime(t, tgA); rt["mute_code"] != "account_suspended" {
		t.Fatalf("mute_code = %v", rt["mute_code"])
	}
	if got := metricInt(t, "requests/upstream_rejection_muted_total"); got != before+1 {
		t.Fatalf("upstream_rejection_muted_total = %d, want %d", got, before+1)
	}

	fromA := w.fakeA.LastSeq()
	for i := 0; i < 4; i++ {
		_, key := client(t, nil, []string{p})
		if resp, body := chat(t, key, alias, "hi"); resp.StatusCode != http.StatusOK {
			t.Fatalf("follow-up %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if n := hits(w.fakeA, kA, fromA); n != 0 {
		t.Fatalf("cooling target was called %d more times", n)
	}

	// Manual recovery puts the target back into rotation at once.
	if code, body := w.adminDo("POST", "/admin/data/targets/"+tgA+"/unmute", nil); code != http.StatusOK {
		t.Fatalf("unmute: %d %s", code, body)
	}
	if rt := targetRuntime(t, tgA); rt["phase"] != "ready" || rt["mute_reason"] != nil {
		t.Fatalf("after unmute runtime = %v", rt)
	}
}

// DeepSeek "Insufficient Balance" on a single-key target used to pass
// through; it now fails over and cools the target.
func TestPoolSingleKeyDeepSeek402FailsOverE2E(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-402")
	kA, kB := uniq("sk-402-a"), uniq("sk-402-b")
	tgA := keyedTarget(t, w.urlA, "/deepseek/v1", alias, []string{kA}, map[string]any{"pool": p, "usage_adapter": "deepseek"})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Preset: "deepseek_402"})
	defer w.fakeA.SetKeyScenario(kA, nil)

	rejectThenServe(t, p, alias, kA, kB)
	assertRejectionCooldown(t, tgA, http.StatusPaymentRequired)
}

// Counterexample: a content-moderation 403 is about the request, so it is
// passed through and the target stays healthy.
func TestPoolModeration403PassesThroughE2E(t *testing.T) {
	p := pool(t)
	alias := uniq("e2e-403mod")
	kA, kB := uniq("sk-403m-a"), uniq("sk-403m-b")
	tgA := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, map[string]any{"pool": p})
	keyedTarget(t, w.urlB, "/openai/v1", alias, []string{kB}, map[string]any{"pool": p})
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusForbidden, Body: `{"error":{"code":"content_policy_violation","message":"blocked"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	for attempt := 0; attempt < 16; attempt++ {
		_, key := client(t, nil, []string{p})
		fromA, fromB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
		resp, body := chat(t, key, alias, "hi")
		if hits(w.fakeA, kA, fromA) == 0 {
			continue
		}
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "content_policy_violation") {
			t.Fatalf("client saw %d %s; want the moderation 403", resp.StatusCode, body)
		}
		if hits(w.fakeB, kB, fromB) != 0 {
			t.Fatalf("moderation 403 crossed to B")
		}
		if rt := targetRuntime(t, tgA); rt["phase"] == "muted" {
			t.Fatalf("moderation 403 muted the target: %v", rt)
		}
		return
	}
	t.Fatalf("no fresh client was routed to the moderation target first")
}

// With every candidate cooling, the client sees the real rejection status
// and reason instead of a bare 503.
func TestAllCoolingReturnsRejectionReasonE2E(t *testing.T) {
	alias := uniq("e2e-403only")
	kA := uniq("sk-403o-a")
	tgA := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{kA}, nil)
	_, key := client(t, []string{tgA}, nil)
	w.fakeA.SetKeyScenario(kA, &fakellm.Scenario{Status: http.StatusForbidden, Body: `{"error":{"type":"permission_error","code":"account_suspended","message":"suspended"}}`})
	defer w.fakeA.SetKeyScenario(kA, nil)

	if resp, body := chat(t, key, alias, "hi"); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "suspended") {
		t.Fatalf("first: %d %s; want the real upstream 403", resp.StatusCode, body)
	}
	from := w.fakeA.LastSeq()
	resp, body := chat(t, key, alias, "hi")
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second: %d retry-after=%q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if !strings.Contains(string(body), `"upstream_rejected"`) || !strings.Contains(string(body), "account_suspended") {
		t.Fatalf("cooldown body = %s", body)
	}
	if hits(w.fakeA, kA, from) != 0 {
		t.Fatalf("cooling target was called again")
	}
}
