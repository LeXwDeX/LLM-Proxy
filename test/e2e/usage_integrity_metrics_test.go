//go:build e2e

package e2e

// Issue #125 acceptance requires a test-instance readback of the fast-path and
// retention metrics. This uses the shared real-proxy world, waits for the
// startup classification and retention snapshot, forces one strict quota read,
// and asserts the observable counters instead of treating the unit tests as
// instance evidence.

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsageIntegrityMetricsReadback(t *testing.T) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		metrics := readMetrics(t)
		integrity, _ := metrics["usage_integrity"].(map[string]any)
		retention, _ := metrics["usage_retention"].(map[string]any)
		proofIndex, _ := metrics["usage_proof_index"].(map[string]any)
		if integrity["complete"] == true && retention["available"] == true && proofIndex["ready"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics not ready: integrity=%v retention=%v proof_index=%v", integrity, retention, proofIndex)
		}
		time.Sleep(100 * time.Millisecond)
	}

	before := fastReads(t)
	var after float64
	// The display aggregate no longer walks the integrity classification (#242).
	// A client with recorded usage whose plan is re-saved gets a strict quota
	// evaluation, which must use the fast path.
	_, up, _, _ := compressionUpstream(t)
	alias := uniq("e2e-metrics")
	tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": up.URL + "/v1", "api_key": uniq("sk-fake-metrics"), "model_mappings": []any{
		map[string]any{"upstream": "Unfamiliar-Upstream", "fallback": alias},
	}})
	name, key := client(t, []string{tg}, nil)
	if response, raw := call(t, key, "POST", "/v1/chat/completions", expand(chatBody("[[fake usage.in=7 usage.out=2]]", false), alias), nil); response.StatusCode != 200 {
		t.Fatalf("seed request HTTP %d: %s", response.StatusCode, raw)
	}
	compressionEvents(t, name, 1)
	for attempt := 0; attempt < 10; attempt++ {
		grantQuota(t, name, []string{tg}, nil)
		waitQuotaRefresh(t)
		after = fastReads(t)
		if after > before {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if after <= before {
		t.Fatalf("usage_integrity.fast_reads did not increase: before=%v after=%v", before, after)
	}
	metrics := readMetrics(t)
	integrity := metrics["usage_integrity"].(map[string]any)
	fallbacks, ok := integrity["fallback_reads"].(map[string]any)
	if !ok {
		t.Fatalf("usage_integrity.fallback_reads missing: %v", integrity["fallback_reads"])
	}
	if _, ok := fallbacks["client_scope_fallback"]; !ok {
		t.Fatalf("fallback_reads lacks client_scope_fallback: %v", fallbacks)
	}
	if retention, _ := metrics["usage_retention"].(map[string]any); retention["available"] != true {
		t.Fatalf("usage_retention unavailable: %v", retention)
	}
}

func readMetrics(t *testing.T) map[string]any {
	t.Helper()
	code, body := w.adminDo("GET", "/admin/metrics", nil)
	if code != 200 {
		t.Fatalf("/admin/metrics: %d %s", code, body)
	}
	var metrics map[string]any
	if err := json.Unmarshal(body, &metrics); err != nil {
		t.Fatalf("decode /admin/metrics: %v", err)
	}
	return metrics
}

func fastReads(t *testing.T) float64 {
	t.Helper()
	integrity, _ := readMetrics(t)["usage_integrity"].(map[string]any)
	fast, _ := integrity["fast_reads"].(float64)
	return fast
}
