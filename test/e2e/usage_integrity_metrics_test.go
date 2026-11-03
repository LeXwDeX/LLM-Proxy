//go:build e2e

package e2e

// Issue #125 acceptance requires a test-instance readback of the fast-path and
// retention metrics. This uses the shared real-proxy world, waits for the
// startup classification and retention snapshot, forces one aggregate read,
// and asserts the observable counters instead of treating the unit tests as
// instance evidence.

import (
	"encoding/json"
	"net/url"
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
	for attempt := 0; attempt < 10; attempt++ {
		now := time.Now().UTC()
		q := url.Values{
			"group_by": {"hour"},
			"from":     {now.Add(-time.Hour + time.Duration(attempt)*time.Second).Format(time.RFC3339)},
			"to":       {now.Format(time.RFC3339)},
		}
		if code, body := w.adminDo("GET", "/admin/data/usage/aggregate?"+q.Encode(), nil); code != 200 {
			t.Fatalf("aggregate readback: %d %s", code, body)
		}
		after = fastReads(t)
		if after > before {
			break
		}
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
