package quota

import (
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// 冷启动后请求路径校准有超时上限；超时按校准失败处理（拒绝，不放行），
// 后台校准完成后自动收敛。
func TestRequestPathCalibrationTimeoutBlocksAndConverges(t *testing.T) {
	db := testOpenDB(t)
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	m := newTestManager(t, db)

	old := calibrationRequestTimeout
	calibrationRequestTimeout = 80 * time.Millisecond
	defer func() { calibrationRequestTimeout = old }()

	release := make(chan struct{})
	var once sync.Once
	m.evaluateHook = func(name string, retry bool) {
		<-release
		m.evaluate(name, retry)
	}

	start := time.Now()
	info, exceeded := m.CheckTarget("alice", "t", "m")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("first request waited %v, want ~timeout", d)
	}
	if !exceeded || info.Scope != ScopeAccounting {
		t.Fatalf("timed-out calibration must reject as accounting-unavailable, got %+v exceeded=%v", info, exceeded)
	}
	if m.Status("alice").CalibrationError == "" {
		t.Fatal("timeout must be recorded as a calibration failure")
	}

	// 后续请求在退避期内不再各自等待超时。
	start = time.Now()
	if _, exceeded := m.CheckTarget("alice", "t", "m"); !exceeded {
		t.Fatal("second request must still be rejected while calibration is pending")
	}
	if d := time.Since(start); d > 40*time.Millisecond {
		t.Fatalf("second request waited %v; waiters must not each pay the timeout", d)
	}

	once.Do(func() { close(release) })
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.RLock()
		ready := m.counters["alice"] != nil && m.calibrationErrors["alice"] == ""
		inflight := len(m.calibrationInFlight)
		m.mu.RUnlock()
		if ready && inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background calibration did not converge after release")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, exceeded := m.CheckTarget("alice", "t", "m"); exceeded {
		t.Fatal("calibrated client must be admitted after convergence")
	}
}
