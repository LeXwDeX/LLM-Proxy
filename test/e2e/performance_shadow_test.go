//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

func intPtr(n int) *int { return &n }

// perfScenario builds a streaming scenario with a fixed output token count.
func perfScenario(delayMS, chunks, chunkDelayMS, output int) *fakellm.Scenario {
	return &fakellm.Scenario{DelayMS: delayMS, Chunks: chunks, ChunkDelayMS: chunkDelayMS,
		Usage: &fakellm.UsageSpec{Output: intPtr(output)}}
}

// perfPoolPair creates a load pool with one fake-A member and one fake-B member
// and installs the given per-key scenarios.
func perfPoolPair(t *testing.T, model string, fastSc, slowSc *fakellm.Scenario) (poolName, fastTarget, slowTarget, fastKey, slowKey string) {
	t.Helper()
	poolName = routingPool(t, "load")
	fastKey, slowKey = uniq("sk-fake-perf"), uniq("sk-fake-perf")
	fastTarget = target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA + "/v1", "api_key": fastKey,
		"pool": poolName, "model_mappings": []any{map[string]any{"upstream": model}}})
	slowTarget = target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlB + "/v1", "api_key": slowKey,
		"pool": poolName, "model_mappings": []any{map[string]any{"upstream": model}}})
	if fastSc != nil {
		w.fakeA.SetKeyScenario(fastKey, fastSc)
		t.Cleanup(func() { w.fakeA.SetKeyScenario(fastKey, nil) })
	}
	if slowSc != nil {
		w.fakeB.SetKeyScenario(slowKey, slowSc)
		t.Cleanup(func() { w.fakeB.SetKeyScenario(slowKey, nil) })
	}
	return poolName, fastTarget, slowTarget, fastKey, slowKey
}

// perfIP returns a unique client IP; the proxy trusts X-Forwarded-For from the
// loopback test peer, so every request avoids IP affinity.
func perfIP() string {
	n := names.Add(1)
	return fmt.Sprintf("10.77.%d.%d", n/250, n%250+1)
}

// perfCall sends one Chat request with a fresh client IP. An empty targetName
// leaves the selection to the pool.
func perfCall(t *testing.T, key, model, targetName string, stream bool) {
	t.Helper()
	hdr := map[string]string{"X-Forwarded-For": perfIP()}
	if targetName != "" {
		hdr["X-Proxy-Target"] = targetName
	}
	body := map[string]any{"model": model, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": "performance"}}}
	resp, raw := call(t, key, "POST", "/v1/chat/completions", body, hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("perf request: HTTP %d %s", resp.StatusCode, raw)
	}
}

// canceledPerfCall starts a stream with a fresh IP and drops it after timeout.
// It returns once the client-side read fails.
func canceledPerfCall(t *testing.T, key, model, targetName string, timeout time.Duration) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	body := map[string]any{"model": model, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "slow first token"}}}
	req, _ := http.NewRequestWithContext(ctx, "POST", w.base+"/v1/chat/completions", strings.NewReader(string(mustJSON(body))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Forwarded-For", perfIP())
	if targetName != "" {
		req.Header.Set("X-Proxy-Target", targetName)
	}
	start := time.Now()
	resp, err := (&http.Client{}).Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("request canceled after %v returned without error", timeout)
	}
	return time.Since(start)
}

// modelPerformance returns the metricsTargetRuntime.model_performance entry of
// target whose upstream model is model, or nil while no sample exists.
func modelPerformance(t *testing.T, target, model string) map[string]any {
	t.Helper()
	list, _ := readMetrics(t)["targets_runtime"].([]any)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok || !strings.EqualFold(entry["name"].(string), target) {
			continue
		}
		perfs, _ := entry["model_performance"].([]any)
		for _, raw := range perfs {
			perf, _ := raw.(map[string]any)
			if perf != nil && strings.EqualFold(fmt.Sprint(perf["upstream_model"]), model) {
				return perf
			}
		}
	}
	return nil
}

// waitModelPerformance polls until match accepts the target's estimate.
func waitModelPerformance(t *testing.T, target, model, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		perf := modelPerformance(t, target, model)
		if perf != nil && match(perf) {
			return perf
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: model_performance never satisfied %s; have %v", target, what, perf)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func perfNum(perf map[string]any, path string) float64 {
	v, _ := dig(perf, path).(float64)
	return v
}

// shadowPoolStats returns the /admin/metrics shadow_routing entry of pool, or
// nil when the pool has no comparison.
func shadowPoolStats(t *testing.T, poolName string) map[string]any {
	t.Helper()
	list, _ := readMetrics(t)["shadow_routing"].([]any)
	for _, item := range list {
		stats, ok := item.(map[string]any)
		if ok && strings.EqualFold(fmt.Sprint(stats["pool"]), poolName) {
			return stats
		}
	}
	return nil
}

// A fast and a slow pool member are summarised with distinguishable TTFT and
// TPS, and a non-streaming reply lands only in the separate non_stream totals.
func TestPerformanceMetricsDistinguishFastAndSlowE2E(t *testing.T) {
	model := uniq("e2e-perf-model")
	fastSc := perfScenario(50, 8, 10, 80)
	slowSc := perfScenario(600, 8, 120, 80)
	_, fast, slow, _, _ := perfPoolPair(t, model, fastSc, slowSc)
	_, key := client(t, []string{fast, slow}, nil)

	for i := 0; i < 7; i++ {
		perfCall(t, key, model, fast, true)
		perfCall(t, key, model, slow, true)
	}
	perfCall(t, key, model, fast, false)

	fastPerf := waitModelPerformance(t, fast, model, "seven streaming samples", func(p map[string]any) bool {
		return p["insufficient"] == false && perfNum(p, "non_stream/samples") >= 1
	})
	slowPerf := waitModelPerformance(t, slow, model, "seven streaming samples", func(p map[string]any) bool {
		return p["insufficient"] == false
	})
	for _, p := range []map[string]any{fastPerf, slowPerf} {
		if got := perfNum(p, "ttft/samples"); got < 5 || got > 9 {
			t.Fatalf("ttft samples = %v, want seven effective samples", got)
		}
		if got := perfNum(p, "tps/samples"); got < 5 || got > 9 {
			t.Fatalf("tps samples = %v, want seven effective samples", got)
		}
	}
	fastTTFT, slowTTFT := perfNum(fastPerf, "ttft/p50"), perfNum(slowPerf, "ttft/p50")
	fastTPS, slowTPS := perfNum(fastPerf, "tps/p50"), perfNum(slowPerf, "tps/p50")
	if fastTTFT >= slowTTFT {
		t.Fatalf("fast member TTFT %v is not lower than slow member %v", fastTTFT, slowTTFT)
	}
	if fastTPS <= slowTPS {
		t.Fatalf("fast member TPS %v is not higher than slow member %v", fastTPS, slowTPS)
	}
	if fastTTFT > 300 || slowTTFT < 400 {
		t.Fatalf("TTFT estimates are not the configured regimes: fast=%v slow=%v", fastTTFT, slowTTFT)
	}
	if got := perfNum(fastPerf, "non_stream/avg_output_tokens"); got < 79.5 || got > 80.5 {
		t.Fatalf("non-stream average output tokens = %v, want about 80", got)
	}
	if got := perfNum(slowPerf, "non_stream/samples"); got != 0 {
		t.Fatalf("slow member non_stream samples = %v, want 0", got)
	}
	if got := perfNum(fastPerf, "penalty_samples"); got != 0 {
		t.Fatalf("fast member penalty_samples = %v, want 0", got)
	}
}

// A client that disconnects during a long first-token wait is counted as a fast
// cancel before the threshold and as a penalty after it; the penalty stays out
// of the TTFT and TPS percentiles.
func TestPerformanceCancelPenaltyE2E(t *testing.T) {
	model := uniq("e2e-perf-cancel-model")
	normal := perfScenario(50, 4, 10, 80)
	_, _, slow, _, slowKey := perfPoolPair(t, model, normal, normal)
	_, key := client(t, []string{slow}, nil)

	// Establish stable percentiles first: the penalty must not move them.
	for i := 0; i < 6; i++ {
		perfCall(t, key, model, slow, true)
	}
	before := waitModelPerformance(t, slow, model, "warm-up samples", func(p map[string]any) bool {
		return perfNum(p, "ttft/samples") >= 5 && perfNum(p, "tps/samples") >= 5
	})
	ttftBefore, tpsBefore := perfNum(before, "ttft/p50"), perfNum(before, "tps/p50")

	// Stream is open, but the first event is delayed far past the client
	// timeout: the attempt waits without content and ends canceled.
	w.fakeB.SetKeyScenario(slowKey, &fakellm.Scenario{FirstContentDelayMS: 8000, Chunks: 4,
		Usage: &fakellm.UsageSpec{Output: intPtr(80)}})
	t.Cleanup(func() { w.fakeB.SetKeyScenario(slowKey, nil) })

	if elapsed := canceledPerfCall(t, key, model, slow, 1*time.Second); elapsed > 3*time.Second {
		t.Fatalf("1s client cancel took %v", elapsed)
	}
	waitModelPerformance(t, slow, model, "one fast cancel", func(p map[string]any) bool {
		return perfNum(p, "fast_cancel_samples") >= 1
	})
	if got := perfNum(modelPerformance(t, slow, model), "penalty_samples"); got != 0 {
		t.Fatalf("a 1s cancel produced %v penalty samples, want 0 before the 3s threshold", got)
	}

	if elapsed := canceledPerfCall(t, key, model, slow, 4*time.Second); elapsed > 6*time.Second {
		t.Fatalf("4s client cancel took %v", elapsed)
	}
	after := waitModelPerformance(t, slow, model, "one penalty sample", func(p map[string]any) bool {
		return perfNum(p, "penalty_samples") >= 1 && perfNum(p, "penalty_ttft/samples") >= 1
	})
	if got := perfNum(after, "penalty_ttft/p50"); got < 3000 {
		t.Fatalf("penalty TTFT %v, want the >=3s lower bound", got)
	}
	// The only difference allowed is histogram decay of the existing samples.
	if delta := perfNum(after, "ttft/samples") - perfNum(before, "ttft/samples"); delta < -1 || delta > 0 || perfNum(after, "ttft/p50") != ttftBefore {
		t.Fatalf("penalty polluted TTFT: before %v/%v after %v/%v",
			perfNum(before, "ttft/samples"), ttftBefore, perfNum(after, "ttft/samples"), perfNum(after, "ttft/p50"))
	}
	if perfNum(after, "tps/p50") != tpsBefore {
		t.Fatalf("penalty polluted TPS: before %v after %v", tpsBefore, perfNum(after, "tps/p50"))
	}
}

// The performance estimates never change the actual selection: a load pool with
// a clearly faster member still alternates between its members, while the
// shadow comparison consistently picks the fast one.
func TestShadowRoutingKeepsLoadBalancingE2E(t *testing.T) {
	model := uniq("e2e-shadow-model")
	fastSc := perfScenario(50, 8, 10, 80)
	slowSc := perfScenario(600, 8, 120, 80)
	poolName, fast, slow, _, _ := perfPoolPair(t, model, fastSc, slowSc)
	_, key := client(t, nil, []string{poolName})

	seqA, seqB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
	for i := 0; i < 12; i++ {
		perfCall(t, key, model, "", true)
	}
	warmA, warmB := len(w.fakeA.RequestsSince(seqA)), len(w.fakeB.RequestsSince(seqB))
	if warmA+warmB != 12 || warmA < 4 || warmB < 4 {
		t.Fatalf("warm-up picks = %d/%d, want a balanced load split", warmA, warmB)
	}
	waitModelPerformance(t, fast, model, "sufficient warm-up", func(p map[string]any) bool {
		return p["insufficient"] == false
	})
	waitModelPerformance(t, slow, model, "sufficient warm-up", func(p map[string]any) bool {
		return p["insufficient"] == false
	})

	// Warm-up may already have recorded a comparison once one member became
	// sufficient; the measurement assertions use deltas from here.
	before := shadowPoolStats(t, poolName)
	beforeAgree, beforeDisagree := 0.0, 0.0
	if before != nil {
		beforeAgree, beforeDisagree = perfNum(before, "agree"), perfNum(before, "disagree")
	}
	seqA, seqB = w.fakeA.LastSeq(), w.fakeB.LastSeq()
	for i := 0; i < 10; i++ {
		perfCall(t, key, model, "", true)
	}
	gotA, gotB := len(w.fakeA.RequestsSince(seqA)), len(w.fakeB.RequestsSince(seqB))
	if gotA+gotB != 10 {
		t.Fatalf("measured upstream requests = %d, want 10", gotA+gotB)
	}
	if gotA < 3 || gotB < 3 || gotA-gotB > 4 || gotB-gotA > 4 {
		t.Fatalf("actual picks = fast %d / slow %d, want a balanced load split", gotA, gotB)
	}

	var stats map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats = shadowPoolStats(t, poolName)
		if stats != nil && perfNum(stats, "agree")+perfNum(stats, "disagree") >= beforeAgree+beforeDisagree+10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shadow_routing never counted 10 measured decisions: %v", stats)
		}
		time.Sleep(50 * time.Millisecond)
	}
	agree, disagree := perfNum(stats, "agree")-beforeAgree, perfNum(stats, "disagree")-beforeDisagree
	// The shadow always prefers the fast member, so its picks match exactly the
	// requests the actual load balancing sent to fast.
	if agree+disagree != 10 {
		t.Fatalf("shadow decisions = %v, want 10", agree+disagree)
	}
	if int(agree) != gotA || int(disagree) != gotB {
		t.Fatalf("shadow agree/disagree = %v/%v, but actual picks were %d/%d", agree, disagree, gotA, gotB)
	}
	// Member shares are cumulative (the warm-up comparison may have landed on
	// either member, depending on the round-robin phase), so the measurement
	// assertion uses shadow_selections deltas; the shares still sum to one.
	members := func(stats map[string]any) (map[string]float64, map[string]float64) {
		shares, selections := map[string]float64{}, map[string]float64{}
		list, _ := stats["members"].([]any)
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			name := strings.ToLower(fmt.Sprint(m["target"]))
			shares[name] = perfNum(m, "share")
			selections[name] = perfNum(m, "shadow_selections")
		}
		return shares, selections
	}
	shares, selections := members(stats)
	_, beforeSelections := members(before) // lenient with a nil baseline
	fastName, slowName := strings.ToLower(fast), strings.ToLower(slow)
	if got := selections[fastName] - beforeSelections[fastName]; got != 10 {
		t.Fatalf("shadow picked fast %v times during measurement, want 10", got)
	}
	if got := selections[slowName] - beforeSelections[slowName]; got != 0 {
		t.Fatalf("shadow picked slow %v times during measurement, want 0", got)
	}
	if sum := shares[fastName] + shares[slowName]; sum < 0.999999 || sum > 1.000001 {
		t.Fatalf("shadow member shares sum to %v, want 1", sum)
	}
}

// A cost-priority pool never records shadow comparisons, exactly like the
// existing load-ignores-prices counter-example: prices keep choosing its member.
func TestShadowRoutingSkipsCostPoolE2E(t *testing.T) {
	model := uniq("e2e-shadow-cost-model")
	poolName := routingPool(t, "cost")
	a := poolMember(t, poolName, w.urlA+"/v1", model)
	b := poolMember(t, poolName, w.urlB+"/v1", model)
	priceTemplate(t, flatPrice(a, model, 1, 1))
	priceTemplate(t, flatPrice(b, model, 1, 1))
	_, key := client(t, nil, []string{poolName})

	for i := 0; i < 3; i++ {
		perfCall(t, key, model, "", true)
	}
	if got := shadowPoolStats(t, poolName); got != nil {
		t.Fatalf("cost pool recorded shadow statistics: %v", got)
	}
}
