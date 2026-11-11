//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// The production incident, end to end. One client uses two models: X (limited
// by the channel's subscription: 429) and Y (healthy). T1 serves both; T2
// serves only Y and rejects every request with a 400 the client cannot
// survive. A 429 of X must not move Y's session to T2, and when Y itself is
// briefly unavailable on T1 the client must be waited for or told to retry,
// never handed T2's 400.

type incidentWorld struct {
	t1, t2         string
	kA, kB         string
	aliasX, aliasY string
	clientName     string
	clientKey      string
}

func newIncidentWorld(t *testing.T) *incidentWorld { return newIncidentWorldWith(t, nil) }

// newIncidentWorldWith adds extra fields (for example max_concurrency or more
// keys) to T1's spec.
func newIncidentWorldWith(t *testing.T, extraT1 map[string]any) *incidentWorld {
	t.Helper()
	iw := &incidentWorld{aliasX: uniq("e2e-x"), aliasY: uniq("e2e-y"), kA: uniq("sk-inc-a"), kB: uniq("sk-inc-b")}
	t1Spec := map[string]any{
		"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": w.urlA + "/openai/v1", "api_key": iw.kA,
		"model_mappings": []any{
			map[string]any{"upstream": "up-" + iw.aliasX, "fallback": iw.aliasX},
			map[string]any{"upstream": "up-" + iw.aliasY, "fallback": iw.aliasY},
		},
	}
	for k, v := range extraT1 {
		t1Spec[k] = v
	}
	iw.t1 = target(t, t1Spec)
	iw.t2 = target(t, map[string]any{
		"endpoint_type": "openai", "usage_adapter": "standard", "endpoint": w.urlB + "/openai/v1", "api_key": iw.kB,
		"model_mappings": []any{map[string]any{"upstream": "up2-" + iw.aliasY, "fallback": iw.aliasY}},
	})
	w.fakeB.SetKeyScenario(iw.kB, &fakellm.Scenario{Status: http.StatusBadRequest, Body: `{"error":{"type":"invalid_request_error","message":"official-400: thinking content missing"}}`})
	t.Cleanup(func() { w.fakeB.SetKeyScenario(iw.kB, nil); w.fakeA.SetKeyScenario(iw.kA, nil) })
	iw.clientName, iw.clientKey = client(t, []string{iw.t1, iw.t2}, nil)
	return iw
}

func (iw *incidentWorld) send(t *testing.T, key, alias, session, content string) (*http.Response, []byte) {
	t.Helper()
	hdr := map[string]string{}
	if session != "" {
		hdr["X-Session-Id"] = session
	}
	return call(t, key, "POST", "/v1/chat/completions", map[string]any{"model": alias,
		"messages": []any{map[string]any{"role": "user", "content": content}}}, hdr)
}

// tripOnT1 makes T1 answer its next request with a 429 (Retry-After as given)
// whatever the model. A request directive would also reach T2 when the request
// is dequeued there, so the one-shot rule sits on T1's key.
func (iw *incidentWorld) tripOnT1(retryAfter string) {
	w.fakeA.SetKeyScenario(iw.kA, &fakellm.Scenario{Status: http.StatusTooManyRequests, RetryAfter: retryAfter, Times: 1})
}

// bind opens the session on T1 (a T2 pick is dequeued by its 400).
func (iw *incidentWorld) bind(t *testing.T, session string) {
	t.Helper()
	resp, body := iw.send(t, iw.clientKey, iw.aliasY, session, "hi")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Proxy-Target") != iw.t1 {
		t.Fatalf("bind: %d target=%q %s, want 200 from %s", resp.StatusCode, resp.Header.Get("X-Proxy-Target"), body, iw.t1)
	}
}

func modelCooldowns(t *testing.T, targetName string) (phase string, models []string) {
	t.Helper()
	runtime, _ := readMetrics(t)["targets_runtime"].([]any)
	for _, e := range runtime {
		entry, _ := e.(map[string]any)
		if entry["name"] != targetName {
			continue
		}
		phase, _ = entry["phase"].(string)
		list, ok := entry["model_cooldowns"].([]any)
		if !ok {
			t.Fatalf("targets_runtime[%s].model_cooldowns missing or not an array: %#v", targetName, entry["model_cooldowns"])
		}
		for _, c := range list {
			models = append(models, c.(map[string]any)["model"].(string))
		}
		return phase, models
	}
	t.Fatalf("target %s missing from targets_runtime", targetName)
	return "", nil
}

func TestModelRateLimitKeepsOtherModelSessionOnTargetE2E(t *testing.T) {
	iw := newIncidentWorld(t)
	session := uniq("sess")
	iw.bind(t, session)

	// X is rate limited by the channel: the client sees the 429 and nothing else.
	resp, body := iw.send(t, iw.clientKey, iw.aliasX, "", "hi [[fake status=429 retry_after=120]]")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("limited model: %d %s, want 429", resp.StatusCode, body)
	}
	phase, models := modelCooldowns(t, iw.t1)
	if phase != "ready" || len(models) != 1 || models[0] != "up-"+iw.aliasX {
		t.Fatalf("T1 phase=%q model cooldowns=%v, want ready with only up-%s", phase, models, iw.aliasX)
	}

	// Y's session stays on T1 and T2's 400 is never produced.
	fromB := w.fakeB.LastSeq()
	for i := 0; i < 3; i++ {
		resp, body := iw.send(t, iw.clientKey, iw.aliasY, session, "again")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Proxy-Target") != iw.t1 {
			t.Fatalf("Y #%d: %d target=%q %s, want 200 from %s", i, resp.StatusCode, resp.Header.Get("X-Proxy-Target"), body, iw.t1)
		}
	}
	if n := hits(w.fakeB, iw.kB, fromB); n != 0 {
		t.Fatalf("T2 received %d requests of the session, want 0", n)
	}

	// X itself is refused fast while it cools (the recovery is beyond the wait bound).
	start := time.Now()
	resp, _ = iw.send(t, iw.clientKey, iw.aliasX, "", "hi")
	ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	if resp.StatusCode != http.StatusServiceUnavailable || ra < 100 || time.Since(start) > 5*time.Second {
		t.Fatalf("X during its cooldown: %d Retry-After=%q after %v, want an immediate 503 with ~120s", resp.StatusCode, resp.Header.Get("Retry-After"), time.Since(start))
	}
}

func TestStickyRequestWaitsForTargetRecoveryE2E(t *testing.T) {
	iw := newIncidentWorld(t)
	session := uniq("sess")
	iw.bind(t, session)
	// Y trips a 2s rate limit on T1; the request that trips it sees the 429.
	iw.tripOnT1("2")
	resp, body := iw.send(t, iw.clientKey, iw.aliasY, session, "trip")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("tripping request: %d %s, want the sticky target's 429", resp.StatusCode, body)
	}
	beforeWait := metricInt(t, "requests/in_proxy_wait_total")
	beforeOK := metricInt(t, "requests/in_proxy_wait_success_total")
	beforeReplayed := metricInt(t, "requests/cached_error_replayed_total")

	// The next request finds T1 cooling for ~2s and T2 rejecting with 400: the
	// platform waits and the client gets T1's answer.
	start := time.Now()
	resp, body = iw.send(t, iw.clientKey, iw.aliasY, session, "next")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Proxy-Target") != iw.t1 {
		t.Fatalf("waited request: %d target=%q %s, want 200 from %s", resp.StatusCode, resp.Header.Get("X-Proxy-Target"), body, iw.t1)
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 10*time.Second {
		t.Fatalf("elapsed %v, want about the 2s cooldown", elapsed)
	}
	if got := metricInt(t, "requests/in_proxy_wait_total"); got != beforeWait+1 {
		t.Fatalf("in_proxy_wait_total = %d, want %d", got, beforeWait+1)
	}
	if got := metricInt(t, "requests/in_proxy_wait_success_total"); got != beforeOK+1 {
		t.Fatalf("in_proxy_wait_success_total = %d, want %d", got, beforeOK+1)
	}
	if got := metricInt(t, "requests/cached_error_replayed_total"); got != beforeReplayed {
		t.Fatalf("T2's 400 was replayed (%d -> %d)", beforeReplayed, got)
	}
}

func TestStickyRequestGetsRetryHintThenReturnsToTargetE2E(t *testing.T) {
	iw := newIncidentWorld(t)
	session := uniq("sess")
	iw.bind(t, session)
	iw.tripOnT1("120")
	if resp, body := iw.send(t, iw.clientKey, iw.aliasY, session, "trip"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("tripping request: %d %s", resp.StatusCode, body)
	}
	beforeHint := metricInt(t, "requests/sticky_retry_hinted_total")
	beforeReplayed := metricInt(t, "requests/cached_error_replayed_total")
	beforeExhausted := metricInt(t, "requests/dequeue_exhausted_total")

	// Too long to wait for: the client is asked to retry, never shown T2's 400.
	resp, body := iw.send(t, iw.clientKey, iw.aliasY, session, "next")
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if resp.StatusCode != http.StatusServiceUnavailable || err != nil || ra < 100 || ra > 121 {
		t.Fatalf("hint: %d Retry-After=%q %s, want 503 with ~120s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if resp.Header.Get("X-Upstream-Status") != "400" || resp.Header.Get("X-Proxy-Target") != "" {
		t.Fatalf("hint headers: X-Upstream-Status=%q X-Proxy-Target=%q body=%s", resp.Header.Get("X-Upstream-Status"), resp.Header.Get("X-Proxy-Target"), body)
	}
	if strings.Contains(string(body), "official-400") || dig(decode(t, body), "error/type") != "upstream_temporarily_unavailable" {
		t.Fatalf("hint body = %s, want the proxy's own error", body)
	}
	if got := metricInt(t, "requests/sticky_retry_hinted_total"); got != beforeHint+1 {
		t.Fatalf("sticky_retry_hinted_total = %d, want %d", got, beforeHint+1)
	}
	if got := metricInt(t, "requests/cached_error_replayed_total"); got != beforeReplayed {
		t.Fatalf("cached_error_replayed_total moved: %d -> %d", beforeReplayed, got)
	}
	if got := metricInt(t, "requests/dequeue_exhausted_total"); got != beforeExhausted+1 {
		t.Fatalf("dequeue_exhausted_total = %d, want %d", got, beforeExhausted+1)
	}

	// The operator clears the cooldown (or it ends): the session is back on T1.
	if code, body := w.adminDo("POST", "/admin/data/targets/"+iw.t1+"/unmute", nil); code >= 300 {
		t.Fatalf("unmute: %d %s", code, body)
	}
	resp, body = iw.send(t, iw.clientKey, iw.aliasY, session, "back")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Proxy-Target") != iw.t1 {
		t.Fatalf("after recovery: %d target=%q %s", resp.StatusCode, resp.Header.Get("X-Proxy-Target"), body)
	}
}

// Without any binding the first real 4xx is still the answer: a bad request
// must not be retried forever.
func TestUnboundRequestStillGetsFirstRealErrorE2E(t *testing.T) {
	iw := newIncidentWorld(t)
	// Another client's session trips Y on T1.
	_, other := client(t, []string{iw.t1, iw.t2}, nil)
	iw.tripOnT1("120")
	iw.send(t, other, iw.aliasY, uniq("sess"), "trip")
	// A fresh client without session or IP binding.
	_, fresh := client(t, []string{iw.t1, iw.t2}, nil)
	resp, body := iw.send(t, fresh, iw.aliasY, "", "hi")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "official-400") {
		t.Fatalf("unbound request: %d %s, want T2's own 400 replayed", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Proxy-Target") != iw.t2 {
		t.Fatalf("X-Proxy-Target = %q, want %s", resp.Header.Get("X-Proxy-Target"), iw.t2)
	}
}

// A sticky target at its hard capacity is unavailable for now: the substitute's
// 400 is not its verdict, so the client is told to retry in a second.
func TestStickyRequestOnFullTargetGetsRetryHintE2E(t *testing.T) {
	iw := newIncidentWorldWith(t, map[string]any{"max_concurrency": 1})
	session := uniq("sess")
	iw.bind(t, session)
	// The bind request releases its slot after the client got the response.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if runtime, _ := readMetrics(t)["targets_runtime"].([]any); inFlight(runtime, iw.t1) == 0 {
			break
		}
	}
	// Occupy T1's only slot with a slow request addressed to it.
	w.fakeA.SetKeyScenario(iw.kA, &fakellm.Scenario{DelayMS: 2500})
	t.Cleanup(func() { w.fakeA.SetKeyScenario(iw.kA, nil) })
	slow := make(chan int, 1)
	go func() {
		resp, sb := call(t, iw.clientKey, "POST", "/v1/chat/completions", map[string]any{"model": iw.aliasY,
			"messages": []any{map[string]any{"role": "user", "content": "slow"}}}, map[string]string{"X-Proxy-Target": iw.t1})
		_ = sb
		slow <- resp.StatusCode
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime, _ := readMetrics(t)["targets_runtime"].([]any); inFlight(runtime, iw.t1) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	resp, body := iw.send(t, iw.clientKey, iw.aliasY, session, "next")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" || resp.Header.Get("X-Upstream-Status") != "400" {
		t.Fatalf("full sticky target: %d Retry-After=%q X-Upstream-Status=%q %s, want the 503 retry hint", resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("X-Upstream-Status"), body)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a full target was waited for (%v)", time.Since(start))
	}
	if strings.Contains(string(body), "official-400") || dig(decode(t, body), "error/substitute_error_code") != "invalid_request_error" {
		t.Fatalf("hint body = %s", body)
	}
	if code := <-slow; code != http.StatusOK {
		t.Fatalf("slow request status = %d", code)
	}
}

func inFlight(runtime []any, name string) float64 {
	for _, e := range runtime {
		if entry, _ := e.(map[string]any); entry["name"] == name {
			n, _ := entry["in_flight"].(float64)
			return n
		}
	}
	return 0
}

// Multi-key target: a model's 429 limits that (key, model) pair only. The other
// model keeps using the same keys, and once every key is limited for the model
// the target cools for that model, visible in the admin views.
func TestMultiKeyModelRateLimitE2E(t *testing.T) {
	k2 := uniq("sk-inc-a2")
	iw := newIncidentWorldWith(t, map[string]any{"api_keys": []string{k2}})
	resp, body := iw.send(t, iw.clientKey, iw.aliasX, "", "hi [[fake status=429 retry_after=120]]")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("limited model: %d %s, want 429", resp.StatusCode, body)
	}
	phase, models := modelCooldowns(t, iw.t1)
	if phase != "ready" || len(models) != 1 || models[0] != "up-"+iw.aliasX {
		t.Fatalf("T1 phase=%q model cooldowns=%v, want ready with only up-%s", phase, models, iw.aliasX)
	}
	// The same keys serve the other model at once.
	resp, body = call(t, iw.clientKey, "POST", "/v1/chat/completions", map[string]any{"model": iw.aliasY,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, map[string]string{"X-Proxy-Target": iw.t1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other model on the same keys: %d %s", resp.StatusCode, body)
	}
	// Both keys report the limited model, neither is exhausted.
	code, raw := w.adminDo("GET", "/admin/data/targets", nil)
	if code != 200 {
		t.Fatalf("targets: %d %s", code, raw)
	}
	var page struct {
		Targets []struct {
			Name        string `json:"name"`
			KeyStatuses []struct {
				Exhausted   bool `json:"exhausted"`
				ModelLimits []struct {
					Model string `json:"model"`
				} `json:"model_limits"`
			} `json:"key_statuses"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	limited := 0
	for _, tg := range page.Targets {
		if tg.Name != iw.t1 {
			continue
		}
		for _, ks := range tg.KeyStatuses {
			if ks.Exhausted {
				t.Fatalf("a key was exhausted by a model-scoped 429: %+v", ks)
			}
			for _, ml := range ks.ModelLimits {
				if ml.Model == "up-"+iw.aliasX {
					limited++
				}
			}
		}
	}
	if limited != 2 {
		t.Fatalf("%d keys report the limited model, want 2", limited)
	}
}
