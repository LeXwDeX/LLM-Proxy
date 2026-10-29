package quota

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func seedColdAlice(t *testing.T, dailyLimit float64) (*Manager, func(limit float64)) {
	t.Helper()
	db := testOpenDB(t)
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	quotaTestCost(t, db)
	setLimit := func(limit float64) {
		putModelGroup(t, db, config.ModelGroup{Name: "g1", CostCoefficient: 1, DailyUSD: limit, Bindings: []config.QuotaGroupBinding{{Target: "tA"}}})
	}
	setLimit(dailyLimit)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "g1"}})
	// alice 今天已用 1 USD（100000 输入 token x $10/1M）。
	if err := nosql.NewUsageStore(db).Record(usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "tA", Model: "gpt-4o", EndpointType: "openai", InputTokens: 100000, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, db)
	m.refreshDebounce = 20 * time.Millisecond
	return m, setLimit
}

// 审查者复现：冷客户端 alice 校准进行中（尚无计数器），管理员把 g1 日额度从 100 降到
// 0.5。后台刷新按计数器找不到 alice；校准若用旧配置落地计数器，会放行最长约 1 小时。
func TestInFlightCalibrationRespectsConfigChange(t *testing.T) {
	m, setLimit := seedColdAlice(t, 100)
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.afterConfigRead = func(string) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(reached)
			<-release // 此刻校准已按旧限额（100）算完，尚未写入计数器
		}
	}
	type res struct {
		info    ExceededInfo
		blocked bool
	}
	out := make(chan res, 1)
	go func() {
		info, ok := m.CheckTarget("alice", "tA", "gpt-4o")
		out <- res{info, ok}
	}()
	<-reached
	setLimit(0.5)
	m.ScheduleRefresh(RefreshScope{Groups: []string{"g1"}})
	close(release)
	r := <-out
	m.FlushRefresh()
	if !r.blocked || r.info.Scope != ScopeModelGroup {
		t.Fatalf("stale pre-change calibration admitted alice: %+v blocked=%v", r.info, r.blocked)
	}
	if info, ok := m.CheckTarget("alice", "tA", "gpt-4o"); !ok || info.Limit != 0.5 {
		t.Fatalf("counters not on the new limit: %+v ok=%v", info, ok)
	}
}

// 配置持续变化超过重试上限：冷客户端（没有计数器可沿用）按校准失败处理（拒绝），
// 而不是落地旧结果。
func TestEvaluateFailsClosedUnderConfigChurn(t *testing.T) {
	fastChurnTiming(t)
	m, _ := seedColdAlice(t, 100)
	calls := 0
	m.afterConfigRead = func(string) { calls++; m.NoteConfigChange() }
	m.Evaluate("alice")
	if calls != maxConfigRetries {
		t.Fatalf("evaluate attempts = %d, want %d", calls, maxConfigRetries)
	}
	if got := m.Status("alice").CalibrationError; got != errConfigChurn.Error() {
		t.Fatalf("calibration error = %q", got)
	}
	m.mu.RLock()
	_, has := m.counters["alice"]
	m.mu.RUnlock()
	if has {
		t.Fatal("stale counters were written despite config churn")
	}
	if _, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); !blocked {
		t.Fatal("cold client must stay rejected under config churn")
	}
}

func fastChurnTiming(t *testing.T) {
	t.Helper()
	oldBackoff, oldDelay := configRetryBackoff, configChurnRescheduleDelay
	configRetryBackoff = func(int) time.Duration { return time.Millisecond }
	configChurnRescheduleDelay = 20 * time.Millisecond
	t.Cleanup(func() { configRetryBackoff, configChurnRescheduleDelay = oldBackoff, oldDelay })
}

// 复审复现：alice 已校准、后台刷新期间配置持续提交，连续重算被丢弃。已校准客户端必须
// 保留旧计数器继续准入（不是被打成拒绝），churn 停止后延后重算收敛到新配置。
func TestChurnKeepsCalibratedClientAdmittedThenConverges(t *testing.T) {
	fastChurnTiming(t)
	m, setLimit := seedColdAlice(t, 100)
	m.Evaluate("alice")
	if _, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); blocked {
		t.Fatal("fixture: calibrated alice must be admitted")
	}
	var churn atomic.Bool
	churn.Store(true)
	m.afterConfigRead = func(string) {
		if churn.Load() {
			m.NoteConfigChange()
		}
	}
	m.Evaluate("alice")
	if got := m.Status("alice").CalibrationError; got != "" {
		t.Fatalf("calibrated client got calibration error %q under churn", got)
	}
	if _, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); blocked {
		t.Fatal("churn turned a calibrated client into a rejection")
	}
	setLimit(0.5)
	churn.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); blocked && info.Limit == 0.5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deferred recalculation did not converge to the new config")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.FlushRefresh()
}

// 重试不得全程持有 accounting gate：同客户端的 RecordUsage 最多等待一次尝试，而不是累加。
func TestConfigRetryReleasesAccountingGateBetweenAttempts(t *testing.T) {
	fastChurnTiming(t)
	configRetryBackoff = func(int) time.Duration { return 30 * time.Millisecond }
	m, _ := seedColdAlice(t, 100)
	const attemptCost = 60 * time.Millisecond
	var calls atomic.Int32
	entered := make(chan struct{})
	m.afterConfigRead = func(string) { // 在 gate 内执行
		if calls.Add(1) == 1 {
			close(entered)
		}
		time.Sleep(attemptCost)
		if calls.Load() < 4 {
			m.NoteConfigChange()
		}
	}
	done := make(chan struct{})
	go func() { m.Evaluate("alice"); close(done) }()
	<-entered
	start := time.Now()
	ev := usage.Event{RecordID: "r1", Timestamp: time.Now().UTC(), ClientName: "alice", Target: "tA", Model: "gpt-4o", EndpointType: "openai", InputTokens: 1, StatusCode: 200}
	if err := m.RecordUsage(auditRecorderFunc(func(usage.Event) error { return nil }), ev); err != nil {
		t.Fatal(err)
	}
	waited := time.Since(start)
	<-done
	if calls.Load() < 4 {
		t.Fatalf("fixture: only %d attempts ran", calls.Load())
	}
	if limit := attemptCost + 80*time.Millisecond; waited > limit {
		t.Fatalf("RecordUsage waited %v, want <= %v (one attempt plus margin, not the sum of all retries)", waited, limit)
	}
}

// Increment 冷启动：读配置期间代次推进，不得用这份可能过期的配置建计数器。
func TestIncrementColdStartSkipsStaleConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		churn    bool
		wantCtrs bool
	}{{"stale generation", true, false}, {"stable generation", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := seedColdAlice(t, 100)
			if tc.churn {
				m.afterColdConfigRead = func() { m.NoteConfigChange() }
			}
			m.Increment("alice", "openai", "gpt-4o", 100000, 0, 0, 0)
			m.mu.RLock()
			_, has := m.counters["alice"]
			m.mu.RUnlock()
			if has != tc.wantCtrs {
				t.Fatalf("counters built = %v, want %v", has, tc.wantCtrs)
			}
		})
	}
}

func TestAffectedClientsIncludesInFlightAndFailedCalibration(t *testing.T) {
	m := seedRefreshFixture(t)
	m.mu.Lock()
	m.calibrationInFlight["cold"] = make(chan struct{})
	m.calibrationErrors["broken"] = "boom"
	m.mu.Unlock()
	got := map[string]bool{}
	for _, n := range m.affectedClients(refreshSet{groups: map[string]struct{}{"nope": {}}}) {
		got[n] = true
	}
	if !got["cold"] || !got["broken"] || len(got) != 2 {
		t.Fatalf("affected = %v, want cold+broken", got)
	}
}

func TestCalibrationPanicFailsClosed(t *testing.T) {
	m, _ := seedColdAlice(t, 100)
	m.evaluateHook = func(string, bool) { panic("db exploded") }
	m.ScheduleRefresh(RefreshScope{Clients: []string{"alice"}})
	m.FlushRefresh() // 不应带崩进程
	if got := m.Status("alice").CalibrationError; got == "" {
		t.Fatal("panic during calibration must be recorded as a calibration failure")
	}
	if _, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); !blocked {
		t.Fatal("client must be rejected after a calibration panic")
	}
}

// 退避期满、校准仍在进行：不再等待（避免每个退避周期一次延迟尖峰）。
func TestExpiredBackoffWithInFlightCalibrationDoesNotWait(t *testing.T) {
	m, _ := seedColdAlice(t, 100)
	old := calibrationRequestTimeout
	calibrationRequestTimeout = 60 * time.Millisecond
	defer func() { calibrationRequestTimeout = old }()
	release := make(chan struct{})
	m.evaluateHook = func(name string, retry bool) { <-release; m.evaluate(name, retry) }
	defer func() { close(release); m.Stop() }()

	if _, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); !blocked {
		t.Fatal("first request must time out and be rejected")
	}
	m.mu.Lock()
	m.calibrationRetryAt["alice"] = time.Now().Add(-time.Second) // 退避已过期
	m.mu.Unlock()
	start := time.Now()
	if _, blocked := m.CheckTarget("alice", "tA", "gpt-4o"); !blocked {
		t.Fatal("still rejected while calibration is running")
	}
	if d := time.Since(start); d > 30*time.Millisecond {
		t.Fatalf("request waited %v for an in-flight calibration after backoff expiry", d)
	}
}

// Stop 等待请求路径的后台校准退出；Stop 之后不再启动新的校准。
func TestStopWaitsForBackgroundCalibration(t *testing.T) {
	m, _ := seedColdAlice(t, 100)
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.counters, "alice") // 让下一次请求重新走冷启动校准
	m.mu.Unlock()
	old := calibrationRequestTimeout
	calibrationRequestTimeout = 40 * time.Millisecond
	defer func() { calibrationRequestTimeout = old }()
	entered, release := make(chan struct{}), make(chan struct{})
	m.evaluateHook = func(string, bool) { close(entered); <-release }
	go m.CheckTarget("alice", "tA", "gpt-4o")
	<-entered
	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a background calibration was still running")
	case <-time.After(80 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after calibration exited")
	}
	called := false
	m.evaluateHook = func(string, bool) { called = true }
	m.CheckTarget("alice", "tA", "gpt-4o")
	if called {
		t.Fatal("calibration started after Stop")
	}
}

// Start/ScheduleRefresh/Stop 并发：-race 下不得出现对 stopCh 的无同步访问。
func TestStartRefreshStopRace(t *testing.T) {
	m, _ := seedColdAlice(t, 100)
	m.evaluateHook = func(string, bool) { time.Sleep(time.Millisecond) }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			m.ScheduleRefresh(RefreshScope{All: true})
			time.Sleep(time.Millisecond)
		}
	}()
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	m.FlushRefresh()
	m.Stop()
}

func TestRefreshStatusReportsPendingAndGeneration(t *testing.T) {
	m := seedRefreshFixture(t)
	gate := make(chan struct{})
	m.evaluateHook = func(string, bool) { <-gate }
	before := m.RefreshStatus()
	m.ScheduleRefresh(RefreshScope{Targets: []string{"tA"}})
	st := m.RefreshStatus()
	if !st.Pending || st.ConfigGeneration != before.ConfigGeneration+1 {
		t.Fatalf("status after schedule = %+v (before %+v)", st, before)
	}
	close(gate)
	m.FlushRefresh()
	if st := m.RefreshStatus(); st.Pending || st.Completed != before.Completed+1 {
		t.Fatalf("status after flush = %+v", st)
	}
}
