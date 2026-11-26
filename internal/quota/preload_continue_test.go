package quota

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func seedPreloadClients(t *testing.T, n int) *Manager {
	t.Helper()
	db := testOpenDB(t)
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelGroup(t, db, config.ModelGroup{Name: "g1", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "tA"}}})
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("c%02d", i)
		putClient(t, db, config.Client{Name: name, AccessKey: "k-" + name, JobLevel: "L5"})
		putClientQuotaGroups(t, db, name, []config.QuotaGroup{{Name: "g1"}})
	}
	return newTestManager(t, db)
}

func waitAllCalibrated(t *testing.T, m *Manager, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ready := 0
		m.mu.RLock()
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("c%02d", i)
			if m.counters[name] != nil && m.calibrationErrors[name] == "" {
				ready++
			}
		}
		m.mu.RUnlock()
		if ready == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d clients calibrated within %v without any request", ready, n, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 预加载超过上限时，Start 不再无限等待，但未评估的客户端必须在后台继续校准，
// 而不是留给首个请求去触发（那会让重启后的首批请求撞上 15s 的校准超时 → 503）。
func TestStartPreloadTimeoutContinuesRemainingInBackground(t *testing.T) {
	const n = 6
	m := seedPreloadClients(t, n)
	old := preloadTimeout
	preloadTimeout = 30 * time.Millisecond
	defer func() { preloadTimeout = old }()

	var mu sync.Mutex
	evaluated := map[string]int{}
	m.evaluateHook = func(name string, retry bool) {
		time.Sleep(100 * time.Millisecond) //单个客户端校准很慢，整体必然超出预加载上限
		m.evaluate(name, retry)
		mu.Lock()
		evaluated[name]++
		mu.Unlock()
	}

	started := time.Now()
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if d := time.Since(started); d > 2*time.Second {
		t.Fatalf("Start blocked %v; it must return once the preload ceiling is hit", d)
	}

	// 没有任何请求：剩余客户端也应被后台预加载完成。
	waitAllCalibrated(t, m, n, 5*time.Second)
	mu.Lock()
	defer mu.Unlock()
	for name, count := range evaluated {
		if count != 1 {
			t.Errorf("client %s evaluated %d times, want exactly once (no duplicate work)", name, count)
		}
	}
}

// Stop 必须等待后台预加载退出，Stop 之后不得再访问库。
func TestStopWaitsForBackgroundPreload(t *testing.T) {
	m := seedPreloadClients(t, 4)
	old := preloadTimeout
	preloadTimeout = 20 * time.Millisecond
	defer func() { preloadTimeout = old }()

	entered, release := make(chan struct{}, 8), make(chan struct{})
	m.evaluateHook = func(name string, retry bool) {
		entered <- struct{}{}
		<-release
		m.evaluate(name, retry)
	}
	startDone := make(chan struct{})
	go func() { _ = m.Start(t.Context()); close(startDone) }()
	<-entered
	close(release) // 让预加载内的在途评估结束，Start 才能返回
	<-startDone

	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	m.mu.RLock()
	inflight := len(m.calibrationInFlight)
	m.mu.RUnlock()
	if inflight != 0 {
		t.Fatalf("calibration still in flight after Stop: %d", inflight)
	}
}
