package proxy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func newShadowTestService(t testing.TB, pools []config.DeploymentPool, targets []config.Target) *Service {
	t.Helper()
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: targets,
		Pools:   pools,
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func shadowTarget(name string, mutate ...func(*config.Target)) config.Target {
	tc := costTarget(name, "ds", "")
	for _, m := range mutate {
		m(&tc)
	}
	return tc
}

func shadowLoadPool() []config.DeploymentPool {
	return []config.DeploymentPool{{Name: "ds", RoutingStrategy: config.PoolRoutingLoad}}
}

// seedStreamSamples records n complete streaming responses with the given TTFT
// and throughput, enough for a sufficient (non-prior) estimate.
func seedStreamSamples(st *targetState, model string, ttft time.Duration, tps float64, n int) {
	tokens := int64(perfMinTPSOutputTokens + 32)
	window := time.Duration(float64(tokens) / tps * float64(time.Second))
	now := time.Now()
	for i := 0; i < n; i++ {
		st.recordPerformance(model, perfObservation{
			StartedAt: now, FinishedAt: now.Add(ttft + window),
			Streaming: true, ContentSeen: true,
			FirstContentAt: now.Add(ttft), LastContentAt: now.Add(ttft + window),
			FinalUsage: true, OutputTokens: tokens, Status: http.StatusOK,
		})
	}
}

func shadowMembers(stats ShadowPoolStats) map[string]ShadowPoolMember {
	members := make(map[string]ShadowPoolMember, len(stats.Members))
	for _, m := range stats.Members {
		members[m.Target] = m
	}
	return members
}

func onlyShadowPool(t *testing.T, s *Service) ShadowPoolStats {
	t.Helper()
	stats := s.ShadowRoutingSnapshot()
	if len(stats) != 1 {
		t.Fatalf("shadow pools = %+v, want exactly one", stats)
	}
	return stats[0]
}

// JSON exposure: the estimate types carry stable snake_case keys and the
// admin layer marshals them without a second contract.
func TestShadowRoutingPerfJSONContract(t *testing.T) {
	est := TargetModelPerf{
		Target:            "t",
		UpstreamModel:     "m",
		TTFT:              PerfEstimate{P50: 75, P90: 150, Samples: 6},
		TPS:               PerfEstimate{P50: 87.5, P90: 400, Samples: 6, Insufficient: true},
		PenaltySamples:    1,
		PenaltyTTFT:       PerfEstimate{P50: 5000, P90: 5500, Samples: 1},
		FastCancelSamples: 2,
		UpdatedAt:         time.Unix(1700000000, 0).UTC(),
		NonStream:         NonStreamPerf{Samples: 1, AvgMs: 2000, AvgOutputTokens: 100},
	}
	raw, err := json.Marshal(est)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"target", "upstream_model", "ttft", "tps", "penalty_samples", "penalty_ttft", "fast_cancel_samples", "insufficient", "updated_at", "non_stream"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("model performance JSON lost key %q", key)
		}
	}
	assertJSONKeys := func(raw json.RawMessage, keys ...string) {
		t.Helper()
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			t.Fatalf("nested decode: %v", err)
		}
		for _, key := range keys {
			if _, ok := nested[key]; !ok {
				t.Fatalf("estimate JSON lost key %q", key)
			}
		}
	}
	assertJSONKeys(top["ttft"], "p50", "p90", "samples", "insufficient")
	assertJSONKeys(top["non_stream"], "samples", "avg_ms", "avg_output_tokens")
}

// The load pool set is collected independently of the cost pool set; the cost
// set keeps exactly its old membership.
func TestShadowRoutingLoadPoolSet(t *testing.T) {
	pools := []config.DeploymentPool{
		{Name: " Perf-A ", RoutingStrategy: " LOAD "},
		{Name: "cost-b", RoutingStrategy: config.PoolRoutingCost},
		{Name: "legacy"},
	}
	load := loadPoolSet(pools)
	if want := map[string]struct{}{"perf-a": {}}; !reflect.DeepEqual(load, want) {
		t.Fatalf("load pools = %v, want %v", load, want)
	}
	cost := costPoolSet(pools)
	if _, ok := cost["perf-a"]; ok {
		t.Fatal("a load pool must not enter the cost pool set")
	}
	for _, name := range []string{"cost-b", "legacy"} {
		if _, ok := cost[name]; !ok {
			t.Fatalf("cost pool set lost %q: %v", name, cost)
		}
	}
	if got := loadPoolSet(nil); len(got) != 0 {
		t.Fatalf("empty config load pools = %v", got)
	}
}

// Both publication paths (CommitPrepared and ApplyConfig) keep loadPools and
// costPools side by side without changing either semantic.
func TestShadowRoutingLoadPoolsFollowConfigReload(t *testing.T) {
	s := newShadowTestService(t, shadowLoadPool(), []config.Target{shadowTarget("perf-a")})
	if !s.performancePriorityPool("ds") {
		t.Fatal("load pool missing after construction")
	}
	if pools := s.costPools.Load(); pools == nil || len(*pools) != 0 {
		t.Fatalf("load pool leaked into cost pools: %v", pools)
	}

	costCfg := &config.Config{
		Targets: []config.Target{shadowTarget("perf-a")},
		Pools:   []config.DeploymentPool{{Name: "ds", RoutingStrategy: config.PoolRoutingCost}},
	}
	prepared, err := s.PrepareConfig(costCfg)
	if err != nil {
		t.Fatalf("PrepareConfig: %v", err)
	}
	s.CommitPrepared(prepared)
	if s.performancePriorityPool("ds") {
		t.Fatal("CommitPrepared kept a pool that became cost-routed")
	}
	if _, ok := (*s.costPools.Load())["ds"]; !ok {
		t.Fatal("CommitPrepared lost the cost pool")
	}

	loadCfg := &config.Config{
		Targets: []config.Target{shadowTarget("perf-a")},
		Pools:   shadowLoadPool(),
	}
	if err := s.ApplyConfig(loadCfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !s.performancePriorityPool("ds") {
		t.Fatal("ApplyConfig did not publish the load pool")
	}
	if pools := s.costPools.Load(); pools == nil || len(*pools) != 0 {
		t.Fatalf("ApplyConfig leaked a load pool into cost pools: %v", pools)
	}
}

// With a fast and a slow member the shadow always picks the fast one while the
// actual load-balanced selection shares both; agree/disagree and the per-member
// shares follow from that.
func TestShadowRoutingCountsAgreement(t *testing.T) {
	s := newShadowTestService(t, shadowLoadPool(),
		[]config.Target{shadowTarget("perf-fast"), shadowTarget("perf-slow")})
	seedStreamSamples(capState(t, s, "perf-fast"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
	seedStreamSamples(capState(t, s, "perf-slow"), costTestModel, 2*time.Second, 5, perfMinSamples+1)

	counts := costPicks(t, s, time.Now(), nil, 10)
	if counts["perf-fast"] == 0 || counts["perf-slow"] == 0 {
		t.Fatalf("actual picks did not share load: %v", counts)
	}

	stats := onlyShadowPool(t, s)
	if stats.Pool != "ds" {
		t.Fatalf("pool = %q", stats.Pool)
	}
	if stats.Agree+stats.Disagree != 10 {
		t.Fatalf("agree+disagree = %d, want 10", stats.Agree+stats.Disagree)
	}
	if stats.Agree != int64(counts["perf-fast"]) || stats.Disagree != int64(counts["perf-slow"]) {
		t.Fatalf("agree/disagree = %d/%d, actual picks = %v", stats.Agree, stats.Disagree, counts)
	}
	members := shadowMembers(stats)
	fast, slow := members["perf-fast"], members["perf-slow"]
	if fast.ShadowSelections != 10 || fast.Share != 1 {
		t.Fatalf("fast member = %+v", fast)
	}
	if slow.ShadowSelections != 0 || slow.Share != 0 {
		t.Fatalf("slow member = %+v", slow)
	}
}

// Shadow counting must not perturb the actual selection: same picks and the
// same rrCounter sequence as an identical pool without a routing strategy.
func TestShadowRoutingDoesNotChangeSelection(t *testing.T) {
	names := []string{"perf-a", "perf-b", "perf-c"}
	withShadowTargets := make([]config.Target, 0, len(names))
	controlTargets := make([]config.Target, 0, len(names))
	for _, name := range names {
		withShadowTargets = append(withShadowTargets, shadowTarget(name))
		controlTargets = append(controlTargets, shadowTarget(name))
	}
	withShadow := newShadowTestService(t, shadowLoadPool(), withShadowTargets)
	control := newShadowTestService(t, nil, controlTargets)
	seedStreamSamples(capState(t, withShadow, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
	seedStreamSamples(capState(t, control, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)

	now := time.Now()
	got := costPicks(t, withShadow, now, nil, 9)
	want := costPicks(t, control, now, nil, 9)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow changed the selection distribution: with=%v without=%v", got, want)
	}
	if withShadow.rrCounter.Load() != control.rrCounter.Load() {
		t.Fatalf("shadow consumed rrCounter: with=%d without=%d", withShadow.rrCounter.Load(), control.rrCounter.Load())
	}
	if len(withShadow.ShadowRoutingSnapshot()) != 1 {
		t.Fatal("shadow did not count where it should")
	}
	if len(control.ShadowRoutingSnapshot()) != 0 {
		t.Fatal("control service recorded shadow statistics")
	}
}

func TestShadowRoutingSkipsExplicitStickyAndFailover(t *testing.T) {
	const model = costTestModel
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected an authenticated principal")
	}
	now := time.Now()
	ip := "192.0.2.1"

	t.Run("explicit target", func(t *testing.T) {
		s := newShadowTestService(t, shadowLoadPool(),
			[]config.Target{shadowTarget("perf-a"), shadowTarget("perf-b")})
		seedStreamSamples(capState(t, s, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
		seedStreamSamples(capState(t, s, "perf-b"), costTestModel, 2*time.Second, 5, perfMinSamples+1)
		state, _, kind, err := s.selectTarget(principal, ip, "", "perf-a", nil, nil, model, costTestPath, now)
		if err != nil || kind != selectionExplicit || state == nil {
			t.Fatalf("explicit selection: state=%v kind=%v err=%v", state, kind, err)
		}
		if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
			t.Fatalf("explicit target recorded shadow statistics: %+v", got)
		}
	})

	t.Run("sticky hit", func(t *testing.T) {
		s := newShadowTestService(t, shadowLoadPool(),
			[]config.Target{shadowTarget("perf-a"), shadowTarget("perf-b")})
		seedStreamSamples(capState(t, s, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
		seedStreamSamples(capState(t, s, "perf-b"), costTestModel, 2*time.Second, 5, perfMinSamples+1)
		s.affinity.Set(affinityKey(ip, principal.Name, model), "perf-b", now)
		state, _, kind, err := s.selectTarget(principal, ip, "", "", nil, nil, model, costTestPath, now)
		if err != nil || kind != selectionAffinityHit || state.Target().Name != "perf-b" {
			t.Fatalf("sticky selection: state=%v kind=%v err=%v", state, kind, err)
		}
		if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
			t.Fatalf("sticky hit recorded shadow statistics: %+v", got)
		}
	})

	t.Run("failover from an unavailable sticky binding", func(t *testing.T) {
		s := newShadowTestService(t, shadowLoadPool(), []config.Target{
			shadowTarget("perf-a"),
			shadowTarget("perf-b"),
			shadowTarget("perf-paused", func(tc *config.Target) { tc.Paused = true }),
		})
		seedStreamSamples(capState(t, s, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
		seedStreamSamples(capState(t, s, "perf-b"), costTestModel, 2*time.Second, 5, perfMinSamples+1)
		s.affinity.Set(affinityKey(ip, principal.Name, model), "perf-paused", now)
		state, _, kind, err := s.selectTarget(principal, ip, "", "", nil, nil, model, costTestPath, now)
		if err != nil || kind != selectionFailover || state == nil {
			t.Fatalf("failover selection: state=%v kind=%v err=%v", state, kind, err)
		}
		if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
			t.Fatalf("failover round recorded shadow statistics: %+v", got)
		}
	})
}

func TestShadowRoutingSkipsCostPools(t *testing.T) {
	s := newShadowTestService(t,
		[]config.DeploymentPool{{Name: "ds", RoutingStrategy: config.PoolRoutingCost}},
		[]config.Target{shadowTarget("perf-fast"), shadowTarget("perf-slow")})
	seedStreamSamples(capState(t, s, "perf-fast"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
	if counts := costPicks(t, s, time.Now(), nil, 4); len(counts) == 0 {
		t.Fatal("no picks")
	}
	if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
		t.Fatalf("cost pool recorded shadow statistics: %+v", got)
	}
}

// Shadow needs at least two admitted members of the selected target's pool;
// one candidate (filtered by ACL or already attempted) records nothing.
func TestShadowRoutingNeedsTwoPoolCandidates(t *testing.T) {
	s := newShadowTestService(t, shadowLoadPool(),
		[]config.Target{shadowTarget("perf-a"), shadowTarget("perf-b")})
	seedStreamSamples(capState(t, s, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
	allowed := map[string]struct{}{"perf-a": {}}
	now := time.Now()
	for i := 0; i < 3; i++ {
		state, lease, _, err := s.selectTarget(nil, "", "", "", allowed, nil, costTestModel, costTestPath, now)
		if err != nil || state.Target().Name != "perf-a" {
			t.Fatalf("pick with one allowed member: state=%v err=%v", state, err)
		}
		state.releaseProbe(lease)
	}
	if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
		t.Fatalf("single admitted member recorded shadow statistics: %+v", got)
	}

	attempted := map[string]struct{}{"perf-b": {}}
	if counts := costPicks(t, s, time.Now(), attempted, 3); len(counts) != 1 || counts["perf-a"] == 0 {
		t.Fatalf("picks with attempted sibling = %v", counts)
	}
	if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
		t.Fatalf("attempted sibling recorded shadow statistics: %+v", got)
	}
}

// A pool where no member has enough samples has nothing to compare: the
// shadow must not count, whether the members were never observed or only have
// insufficient (prior) estimates.
func TestShadowRoutingSkipsPoolWithoutSufficientSamples(t *testing.T) {
	s := newShadowTestService(t, shadowLoadPool(),
		[]config.Target{shadowTarget("perf-a"), shadowTarget("perf-b")})
	if counts := costPicks(t, s, time.Now(), nil, 4); len(counts) == 0 {
		t.Fatal("no picks")
	}
	if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
		t.Fatalf("unobserved pool counted shadow statistics: %+v", got)
	}

	// Observed but below perfMinSamples: still all-insufficient.
	seedStreamSamples(capState(t, s, "perf-a"), costTestModel, 100*time.Millisecond, 100, perfMinSamples-2)
	seedStreamSamples(capState(t, s, "perf-b"), costTestModel, 2*time.Second, 5, perfMinSamples-2)
	if counts := costPicks(t, s, time.Now(), nil, 4); len(counts) == 0 {
		t.Fatal("no picks")
	}
	if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
		t.Fatalf("insufficient-only pool counted shadow statistics: %+v", got)
	}
}

// One sufficient member is enough to compare: insufficient members keep their
// prior in the argmin, and the shadow still picks the fast (sufficient) one.
func TestShadowRoutingCountsWithOneSufficientMember(t *testing.T) {
	s := newShadowTestService(t, shadowLoadPool(),
		[]config.Target{shadowTarget("perf-fast"), shadowTarget("perf-slow")})
	seedStreamSamples(capState(t, s, "perf-fast"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
	seedStreamSamples(capState(t, s, "perf-slow"), costTestModel, 2*time.Second, 5, perfMinSamples-2)

	counts := costPicks(t, s, time.Now(), nil, 6)
	stats := onlyShadowPool(t, s)
	if stats.Agree+stats.Disagree != 6 {
		t.Fatalf("decisions = %d, want 6", stats.Agree+stats.Disagree)
	}
	if stats.Agree != int64(counts["perf-fast"]) || stats.Disagree != int64(counts["perf-slow"]) {
		t.Fatalf("agree/disagree = %d/%d, actual picks = %v", stats.Agree, stats.Disagree, counts)
	}
	members := shadowMembers(stats)
	if members["perf-fast"].ShadowSelections != 6 || members["perf-fast"].Share != 1 {
		t.Fatalf("fast member = %+v", members["perf-fast"])
	}
	if members["perf-slow"].ShadowSelections != 0 {
		t.Fatalf("slow member shadow picks = %d", members["perf-slow"].ShadowSelections)
	}
}

// The snapshot is a copy: mutating it must not change the counters, and
// concurrent selections plus snapshots must pass the race detector.
func TestShadowRoutingSnapshotIsConcurrentCopy(t *testing.T) {
	s := newShadowTestService(t, shadowLoadPool(),
		[]config.Target{shadowTarget("perf-fast"), shadowTarget("perf-slow")})
	seedStreamSamples(capState(t, s, "perf-fast"), costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				state, lease, _, err := s.selectTarget(nil, "", "", "", nil, nil, costTestModel, costTestPath, time.Now())
				if err != nil {
					continue
				}
				state.releaseProbe(lease)
				_ = s.ShadowRoutingSnapshot()
			}
		}()
	}
	wg.Wait()
	total := int64(0)
	for _, pool := range s.ShadowRoutingSnapshot() {
		total += pool.Agree + pool.Disagree
	}
	if total != 40 {
		t.Fatalf("decisions = %d, want 40", total)
	}
	snapshot := onlyShadowPool(t, s)
	snapshot.Members[0].ShadowSelections = -1
	if again := s.ShadowRoutingSnapshot(); again[0].Members[0].ShadowSelections < 0 {
		t.Fatal("snapshot exposed mutable counters")
	}
}

// BenchmarkShadowObserveSelection is the shadow comparison's cost on the hot
// selection path: two estimates plus one counter update, no allocation expected
// beyond map growth on the first calls.
func BenchmarkShadowObserveSelection(b *testing.B) {
	s := newShadowTestService(b, shadowLoadPool(),
		[]config.Target{shadowTarget("perf-fast"), shadowTarget("perf-slow")})
	fast, ok := s.targetByName("perf-fast")
	if !ok {
		b.Fatal("fast target missing")
	}
	slow, ok := s.targetByName("perf-slow")
	if !ok {
		b.Fatal("slow target missing")
	}
	seedStreamSamples(fast, costTestModel, 100*time.Millisecond, 100, perfMinSamples+1)
	seedStreamSamples(slow, costTestModel, 2*time.Second, 5, perfMinSamples+1)
	admitted := []admittedCandidate{
		{state: fast, keyCount: 1, activeCount: 1},
		{state: slow, keyCount: 1, activeCount: 1},
	}
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.observeShadowSelection(fast, admitted, costTestModel, now)
	}
}

// The routing red line: a request without a client model has no per-model
// estimate to compare and must not record shadow statistics.
func TestShadowRoutingSkipsRequestsWithoutModel(t *testing.T) {
	unrestricted := func(name string) config.Target {
		tc := shadowTarget(name)
		tc.ModelMappings = nil
		return tc
	}
	s := newShadowTestService(t, shadowLoadPool(),
		[]config.Target{unrestricted("perf-a"), unrestricted("perf-b")})
	// Even with sufficient estimates recorded under the empty model key, a
	// model-less request has no per-model comparison and must not count.
	seedStreamSamples(capState(t, s, "perf-a"), "", 100*time.Millisecond, 100, perfMinSamples+1)
	seedStreamSamples(capState(t, s, "perf-b"), "", 2*time.Second, 5, perfMinSamples+1)
	state, lease, _, err := s.selectTarget(nil, "", "", "", nil, nil, "", costTestPath, time.Now())
	if err != nil || state == nil {
		t.Fatalf("selection without model: state=%v err=%v", state, err)
	}
	state.releaseProbe(lease)
	if got := s.ShadowRoutingSnapshot(); len(got) != 0 {
		t.Fatalf("model-less selection recorded shadow statistics: %+v", got)
	}
	if strings.TrimSpace(state.Target().Name) == "" {
		t.Fatal("unexpected empty target name")
	}
}
