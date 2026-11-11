package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// /admin/metrics exposes the per-model 429 cooldowns of a target (read-only)
// while the target's phase stays ready: it keeps serving its other models.
func TestMetricsExposeModelCooldowns(t *testing.T) {
	f := newAdminBreakerFixture(t)

	read := func() (runtime []map[string]any, requests map[string]json.RawMessage) {
		t.Helper()
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/metrics: HTTP %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			TargetsRuntime []map[string]any           `json:"targets_runtime"`
			Requests       map[string]json.RawMessage `json:"requests"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.TargetsRuntime, body.Requests
	}
	find := func(rt []map[string]any) map[string]any {
		t.Helper()
		for _, e := range rt {
			if e["name"] == "target-1" {
				return e
			}
		}
		t.Fatal("target-1 missing from targets_runtime")
		return nil
	}

	// No cooldown yet: the field is present and empty.
	rt, requests := read()
	if cds, ok := find(rt)["model_cooldowns"].([]any); !ok || len(cds) != 0 {
		t.Fatalf("idle model_cooldowns = %#v, want []", find(rt)["model_cooldowns"])
	}
	for _, key := range []string{"in_proxy_wait_total", "in_proxy_wait_success_total", "in_proxy_wait_rejected_total", "in_proxy_waiting", "sticky_retry_hinted_total"} {
		if _, ok := requests[key]; !ok {
			t.Fatalf("/metrics requests missing %s", key)
		}
	}

	f.limit.Store(true)
	if rec := f.request(t); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("limited request: HTTP %d %s", rec.Code, rec.Body.String())
	}
	f.limit.Store(false)

	rt, _ = read()
	entry := find(rt)
	if entry["phase"] != "ready" {
		t.Fatalf("phase = %v, want ready while only a model is limited", entry["phase"])
	}
	if _, muted := entry["muted_until"]; muted && entry["muted_until"] != "0001-01-01T00:00:00Z" {
		t.Fatalf("a model 429 must not set muted_until: %v", entry["muted_until"])
	}
	cds, ok := entry["model_cooldowns"].([]any)
	if !ok || len(cds) != 1 {
		t.Fatalf("model_cooldowns = %#v, want one entry", entry["model_cooldowns"])
	}
	cd := cds[0].(map[string]any)
	until, err := time.Parse(time.RFC3339Nano, cd["until"].(string))
	if cd["model"] != "qwen-max" || cd["status"] != float64(429) || cd["reason"] != "upstream_429" || err != nil || time.Until(until) < 100*time.Second {
		t.Fatalf("model cooldown = %#v (until err %v), want qwen-max/429/upstream_429 with the Retry-After of 120s", cd, err)
	}
	// The same entry is on the proxy-level status that healthz/overview render.
	if st := f.status(t); len(st.ModelCooldowns) != 1 || st.Muted {
		t.Fatalf("TargetStatus = %+v", st)
	}
}
