package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// newBreakerState 构建单节点状态供熔断状态机纯单测使用。
func newBreakerState(t *testing.T, threshold int) *targetState {
	t.Helper()
	states, _, err := buildTargetStates([]config.Target{
		{Name: "brk", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("buildTargetStates: %v", err)
	}
	st := states["brk"]
	if st == nil {
		t.Fatal("expected target state brk")
	}
	st.breakerThreshold = threshold
	return st
}

// newSingleTargetService 构建恰好一个节点的服务（验证全不可用 503 与半开占位语义）。
func newSingleTargetService(t *testing.T) *Service {
	t.Helper()
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "only", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k"}},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// newAllowAllPrincipal 构建对全部目标可访问的 principal（显式点名路径需要）。
func newAllowAllPrincipal(t *testing.T, accessKey string) *auth.Principal {
	t.Helper()
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", accessKey)); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	p, ok := store.Authenticate(accessKey)
	if !ok {
		t.Fatal("expected principal")
	}
	return p
}

// TestBreakerConsecutiveFailuresEnterCooldown 验收①：ready 状态下连续 N 次失败才进入
// muted 冷却；冷却期内节点不被调度（主候选循环与显式指定路径均拒绝）。
func TestBreakerConsecutiveFailuresEnterCooldown(t *testing.T) {
	const n = 3
	st := newBreakerState(t, n)
	base := 60 * time.Second
	t0 := time.Now()

	if d, _ := st.admitWithProbeState(t0); d != admitOpen {
		t.Fatalf("fresh state should be admitOpen, got %d", d)
	}

	// 前 N-1 次失败：仍在 ready，保持可调度。
	for i := 1; i < n; i++ {
		st.MarkFailure(t0.Add(time.Duration(i)*time.Second), base)
		if st.IsMuted(t0.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("failure #%d below threshold %d should not enter cooldown", i, n)
		}
		if d, _ := st.admitWithProbeState(t0.Add(time.Duration(i) * time.Second)); d != admitOpen {
			t.Fatalf("failure #%d below threshold should stay admitOpen, got %d", i, d)
		}
	}

	// 第 N 次失败：进入冷却。
	tripAt := t0.Add(time.Duration(n) * time.Second)
	st.MarkFailure(tripAt, base)
	if !st.IsMuted(tripAt.Add(time.Second)) {
		t.Fatalf("%d consecutive failures should trip into cooldown", n)
	}
	if got, want := st.NextRetry(), tripAt.Add(base); !got.Equal(want) {
		t.Fatalf("first trip cooldown = %v, want base %v", got, want)
	}
	if d, _ := st.admitWithProbeState(tripAt.Add(time.Second)); d != admitCooling {
		t.Fatalf("cooldown period should be admitCooling, got %d", d)
	}
}

// TestBreakerCooldownPeriodNotScheduled 服务级验收①：唯一节点处于冷却期时，
// selectTarget 不返回该节点而是报 503（沿用全静默语义），且不产生任何路由结果。
func TestBreakerCooldownPeriodNotScheduled(t *testing.T) {
	svc := newSingleTargetService(t)
	state := svc.targetOrder[0]
	state.breakerThreshold = 1
	base := time.Minute
	now := time.Now()
	state.MarkFailure(now, base)

	p := newAllowAllPrincipal(t, "token")
	inCooldown := now.Add(base / 2)
	st, _, kind, err := svc.selectTarget(p, "10.9.9.1", "", "", nil, nil, "", "/v1/chat/completions", inCooldown)
	if st != nil || err == nil {
		t.Fatalf("muted-only pool must not route, got state=%v kind=%d err=%v", st, kind, err)
	}
	se, ok := err.(*selectionError)
	if !ok || se.Status != http.StatusServiceUnavailable {
		t.Fatalf("expect 503 selectionError, got %#v", err)
	}
	if !strings.Contains(se.Message, "all targets muted until") {
		t.Fatalf("unexpected message %q", se.Message)
	}
}

// TestBreakerHalfOpenProbeSuccessRecoversReady 验收②前半：冷却到期进入 half-open，
// 仅放行一个探测位（并发第二请求被拒）；探测成功后回到 ready 并清零计数、恢复全量放行。
func TestBreakerHalfOpenProbeSuccessRecoversReady(t *testing.T) {
	st := newBreakerState(t, 1)
	base := 30 * time.Second
	t0 := time.Now()

	st.MarkFailure(t0, base)
	expired := t0.Add(base + time.Second)

	d, lease := st.admitWithProbeState(expired)
	if d != admitOpen || lease == nil {
		t.Fatalf("expired cooldown should admit the first probe, got %d", d)
	}
	// 探测在途：第二个请求不得再进入（独占探测位）。
	if d, _ := st.admitWithProbeState(expired); d != admitProbeBusy {
		t.Fatalf("second request during probe should be admitProbeBusy, got %d", d)
	}

	// 探测成功：清零计数回 ready，探测位释放，后续请求全量放行。
	st.MarkSuccess(expired.Add(time.Second))
	st.releaseProbe(lease)
	stats := st.Stats()
	if stats.ConsecutiveFailure != 0 {
		t.Fatalf("success should reset consecutive failures, got %d", stats.ConsecutiveFailure)
	}
	if st.IsMuted(expired.Add(time.Second)) {
		t.Fatal("recovered state must not be muted")
	}
	for i := 0; i < 5; i++ {
		at := expired.Add(time.Duration(i+2) * time.Second)
		if d, _ := st.admitWithProbeState(at); d != admitOpen {
			t.Fatalf("post-recovery admit #%d should be admitOpen, got %d", i, d)
		}
	}
}

// TestBreakerHalfOpenProbeFailureExtendsCooldown 验收②后半：half-open 探测失败回到
// muted 且指数延长冷却（2x、4x、8x 封顶）；任一成功即整体复位为基线冷却系数。
func TestBreakerHalfOpenProbeFailureExtendsCooldown(t *testing.T) {
	st := newBreakerState(t, 1)
	base := time.Minute
	t0 := time.Now()

	st.MarkFailure(t0, base) // 第 1 次失败 → 冷却 = 1x 基线
	cur := t0.Add(base)
	wantExponents := []time.Duration{2 * base, 4 * base, 8 * base, 8 * base}
	for i, want := range wantExponents {
		d, lease := st.admitWithProbeState(cur)
		if d != admitOpen || lease == nil {
			t.Fatalf("probe #%d should win slot after expiry, got %d", i, d)
		}
		st.MarkFailure(cur, base)
		st.releaseProbe(lease)
		got := st.NextRetry().Sub(cur)
		if got != want {
			t.Fatalf("probe failure #%d cooldown = %v, want extended %v", i, got, want)
		}
		cur = cur.Add(want)
	}

	// 恢复路径：任一探测成功后，再次新鲜故障的冷却回到基线（无残留放大）。
	st.MarkSuccess(cur)
	st.MarkFailure(cur.Add(time.Second), base)
	if got := st.NextRetry().Sub(cur.Add(time.Second)); got != base {
		t.Fatalf("fresh failure after recovery should cool down base %v, got %v", base, got)
	}
}

// TestBreakerPausedTargetZeroTraffic 验收③：paused 节点任意状态零流量——ready、
// 冷却、half-open 探测位空闲均不参与调度；移除（未知名）同样不可达。
func TestBreakerPausedTargetZeroTraffic(t *testing.T) {
	targets := []config.Target{
		{Name: "dead-paused", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k", Paused: true},
	}
	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: targets,
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	state := svc.targetOrder[0]

	now := time.Now()
	probePoints := []struct {
		name string
		mut  func()
	}{
		{"ready", func() {}},
		{"tripped-cooling", func() { state.MarkFailure(now, time.Minute) }},
	}
	for _, pp := range probePoints {
		pp.mut()
		p := newAllowAllPrincipal(t, "token")
		st, _, kind, serr := svc.selectTarget(p, "10.5.5.5", "", "", nil, nil, "", "/v1/chat/completions", now)
		if st != nil {
			t.Fatalf("paused target routed in phase %q: kind=%d", pp.name, kind)
		}
		if serr == nil {
			t.Fatalf("paused-only pool should error in phase %q", pp.name)
		}
		if se, ok := serr.(*selectionError); !ok || se.Status != http.StatusServiceUnavailable {
			t.Fatalf("phase %q expect 503, got %#v", pp.name, serr)
		}
	}

	// 显式点名 paused 节点同样拒绝且零流量。
	pausedP := newAllowAllPrincipal(t, "token")
	if _, _, _, err := svc.selectTarget(pausedP, "", "", "dead-paused", nil, nil, "", "/v1/chat/completions", now); err == nil ||
		!strings.Contains(err.Error(), "paused") {
		t.Fatalf("explicit paused target must be rejected, got %v", err)
	}

	// 移除（未配置）的节点完全不可达。
	if _, ok := svc.targetByName("ghost"); ok {
		t.Fatal("removed target must not resolve")
	}
}

// TestBreakerHalfOpenSelectorSingleProbeSlot 选择器级半开验证：唯一节点冷却到期后，
// 第一路选中占用探测位；该探测未完成前第二条自动选路不得命中同一节点（503 恢复中语义）；
// 网络层超时类失败不触碰状态机（由既有 TestServiceTimeoutDoesNotRetryOrMute 守护）。
func TestBreakerHalfOpenSelectorSingleProbeSlot(t *testing.T) {
	svc := newSingleTargetService(t)
	state := svc.targetOrder[0]
	state.breakerThreshold = 1
	base := 20 * time.Millisecond
	t0 := time.Now()
	state.MarkFailure(t0, base)

	p := newAllowAllPrincipal(t, "token")
	expired := t0.Add(base + time.Millisecond)

	first, lease, kind, err := svc.selectTarget(p, "10.7.7.7", "", "", nil, nil, "", "/v1/chat/completions", expired)
	if err != nil || first == nil {
		t.Fatalf("first half-open request should win the probe slot, got err=%v", err)
	}
	if kind != selectionRoundRobin && kind != selectionAffinityHit && kind != selectionFailover {
		t.Fatalf("unexpected kind %d", kind)
	}

	second, _, _, err2 := svc.selectTarget(p, "10.7.7.8", "", "", nil, nil, "", "/v1/chat/completions", expired)
	if second != nil || err2 == nil {
		t.Fatalf("second request during in-flight probe must not route, got state=%v err=%v", second, err2)
	}
	se, ok := err2.(*selectionError)
	if !ok || se.Status != http.StatusServiceUnavailable {
		t.Fatalf("expect 503 selectionError during probe, got %#v", err2)
	}

	// 探测成功释放探测位并复 ready，后续恢复正常。
	state.MarkSuccess(expired.Add(10 * time.Millisecond))
	state.releaseProbe(lease)
	third, _, _, err3 := svc.selectTarget(p, "10.7.7.9", "", "", nil, nil, "", "/v1/chat/completions", expired.Add(20*time.Millisecond))
	if err3 != nil || third == nil {
		t.Fatalf("post-recovery routing should succeed, got err=%v", err3)
	}
}

// TestBreakerHalfOpenProbeLeaseFollowsSelector 双节点回归（#61）：候选过滤期
// admit CAS 抢到的 half-open 探测租约必须由本次选择当场消化——即使加权轮询相位
// 指向他节点，也须优先返回租约持有者。否则租约孤立 breakerProbeLease（2min）：
// 期间显式点名 503、粘连拒绝、自动选路过滤，探针永不发出，恢复非确定。
func TestBreakerHalfOpenProbeLeaseFollowsSelector(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "alpha", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k"},
			{Name: "beta", EndpointType: "openai", Endpoint: "http://127.0.0.1:2", APIKey: "k"},
		},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	alpha := svc.targetOrder[0]
	alpha.breakerThreshold = 1
	t0 := time.Now()
	alpha.MarkFailure(t0, 20*time.Millisecond)
	expired := t0.Add(25 * time.Millisecond)

	// 钉死轮询相位：两节点各权重 1，index=1 落 beta。修复前加权选择会选中 beta，
	// 把 alpha 刚经 CAS 抢到的探测租约孤立——本断言即回归判别器。
	svc.rrCounter.Store(1)

	p := newAllowAllPrincipal(t, "token")
	got, lease, kind, err := svc.selectTarget(p, "10.8.8.1", "", "", nil, nil, "", "/v1/chat/completions", expired)
	if err != nil || got == nil {
		t.Fatalf("half-open probe window must route, got err=%v", err)
	}
	if name := got.Target().Name; name != "alpha" {
		t.Fatalf("probe-granted lease must be selected, got %q", name)
	}
	if kind == selectionExplicit {
		t.Fatalf("probe priority must come from auto selection, got kind %d", kind)
	}

	// 探测在途：后续自动流量分流到健康节点 beta，不得 503。
	second, _, _, err2 := svc.selectTarget(p, "10.8.8.2", "", "", nil, nil, "", "/v1/chat/completions", expired)
	if err2 != nil || second == nil || second.Target().Name != "beta" {
		t.Fatalf("healthy peer must serve during probe, got %v err=%v", second, err2)
	}

	// 探测终态释放探测位后，alpha 回到全量放行。
	alpha.MarkSuccess(expired.Add(time.Millisecond))
	alpha.releaseProbe(lease)
	if d, _ := alpha.admitWithProbeState(expired.Add(2 * time.Millisecond)); d != admitOpen {
		t.Fatalf("post-probe alpha should be admitOpen, got %d", d)
	}
}
