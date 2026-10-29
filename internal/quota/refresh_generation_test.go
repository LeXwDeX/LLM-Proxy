package quota

import (
	"sync"
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

// 配置持续变化超过重试上限：按校准失败处理（拒绝），而不是落地旧结果。
func TestEvaluateFailsClosedUnderConfigChurn(t *testing.T) {
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
