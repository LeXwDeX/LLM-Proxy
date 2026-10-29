package quota

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

type evalRecorder struct {
	mu    sync.Mutex
	names []string
	delay time.Duration
	cur   int32
	max   int32
}

func (r *evalRecorder) hook(name string, retry bool) {
	n := atomic.AddInt32(&r.cur, 1)
	for {
		m := atomic.LoadInt32(&r.max)
		if n <= m || atomic.CompareAndSwapInt32(&r.max, m, n) {
			break
		}
	}
	time.Sleep(r.delay)
	atomic.AddInt32(&r.cur, -1)
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
}

func (r *evalRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.names...)
	sort.Strings(out)
	return out
}

func seedRefreshFixture(t *testing.T) *Manager {
	db := testOpenDB(t)
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L6"})
	putModelGroup(t, db, config.ModelGroup{Name: "g1", CostCoefficient: 1, DailyUSD: 5, Bindings: []config.QuotaGroupBinding{{Target: "tA"}}})
	putModelGroup(t, db, config.ModelGroup{Name: "g2", CostCoefficient: 1, DailyUSD: 5, Bindings: []config.QuotaGroupBinding{{Target: "tB"}}})
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"})
	putClient(t, db, config.Client{Name: "bob", AccessKey: "k2", JobLevel: "L6"})
	putClient(t, db, config.Client{Name: "carol", AccessKey: "k3", JobLevel: "L6"})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "g1"}})
	putClientQuotaGroups(t, db, "bob", []config.QuotaGroup{{Name: "g2"}})
	m := newTestManager(t, db)
	for _, n := range []string{"alice", "bob", "carol"} {
		m.Evaluate(n)
	}
	m.refreshDebounce = 30 * time.Millisecond
	return m
}

// 刷新只覆盖受影响的客户端；连续多次写操作只触发有限次刷新。
func TestScheduleRefreshScopesAndDebounces(t *testing.T) {
	cases := []struct {
		name  string
		scope RefreshScope
		want  []string
	}{
		{"target", RefreshScope{Targets: []string{"TA"}}, []string{"alice"}},
		{"group", RefreshScope{Groups: []string{"g2"}}, []string{"bob"}},
		{"job level", RefreshScope{JobLevels: []string{"l6"}}, []string{"bob", "carol"}},
		{"explicit clients", RefreshScope{Clients: []string{"carol"}}, []string{"carol"}},
		{"all", RefreshScope{All: true}, []string{"alice", "bob", "carol"}},
		{"unrelated", RefreshScope{Targets: []string{"nope"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := seedRefreshFixture(t)
			rec := &evalRecorder{}
			m.evaluateHook = rec.hook
			for i := 0; i < 25; i++ {
				m.ScheduleRefresh(tc.scope)
			}
			m.FlushRefresh()
			got := rec.seen()
			if fmt.Sprint(got) != fmt.Sprint(tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Fatalf("evaluated %v, want %v (each exactly once)", got, tc.want)
			}
		})
	}
}

func TestScheduleRefreshReturnsImmediatelyAndMergesScopes(t *testing.T) {
	m := seedRefreshFixture(t)
	rec := &evalRecorder{delay: 200 * time.Millisecond}
	m.evaluateHook = rec.hook
	start := time.Now()
	m.ScheduleRefresh(RefreshScope{Targets: []string{"tA"}})
	m.ScheduleRefresh(RefreshScope{Groups: []string{"g2"}})
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Fatalf("ScheduleRefresh blocked for %v", d)
	}
	m.FlushRefresh()
	if got := rec.seen(); fmt.Sprint(got) != "[alice bob]" {
		t.Fatalf("merged scopes evaluated %v", got)
	}
}

// 刷新期间到达的新写操作不丢：在运行中再调度，Flush 后也被评估。
func TestScheduleRefreshDuringRunIsNotLost(t *testing.T) {
	m := seedRefreshFixture(t)
	rec := &evalRecorder{delay: 150 * time.Millisecond}
	m.evaluateHook = rec.hook
	m.ScheduleRefresh(RefreshScope{Targets: []string{"tA"}})
	time.Sleep(80 * time.Millisecond) // 刷新已在运行
	m.ScheduleRefresh(RefreshScope{Groups: []string{"g2"}})
	m.FlushRefresh()
	if got := rec.seen(); fmt.Sprint(got) != "[alice bob]" {
		t.Fatalf("evaluated %v", got)
	}
}

func TestScheduleRefreshAllClearsRemovedClients(t *testing.T) {
	m := seedRefreshFixture(t)
	m.mu.Lock()
	m.counters["ghost"] = &clientCounters{}
	m.mu.Unlock()
	m.ScheduleRefresh(RefreshScope{All: true})
	m.FlushRefresh()
	m.mu.RLock()
	_, ok := m.counters["ghost"]
	m.mu.RUnlock()
	if ok {
		t.Fatal("stale client counters survived a full refresh")
	}
}

// 冷启动预加载按有界并发进行：并行但不超过上限，且全部完成。
func TestPreloadUsesBoundedConcurrency(t *testing.T) {
	db := testOpenDB(t)
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	for i := 0; i < 16; i++ {
		putClient(t, db, config.Client{Name: fmt.Sprintf("c%02d", i), AccessKey: fmt.Sprintf("k%d", i), JobLevel: "L5"})
	}
	m := newTestManager(t, db)
	rec := &evalRecorder{delay: 40 * time.Millisecond}
	m.evaluateHook = rec.hook
	start := time.Now()
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if len(rec.seen()) != 16 {
		t.Fatalf("preloaded %d clients, want 16", len(rec.seen()))
	}
	if rec.max < 2 || int(rec.max) > preloadConcurrency {
		t.Fatalf("max concurrency %d, want within [2,%d]", rec.max, preloadConcurrency)
	}
	if d := time.Since(start); d > 16*40*time.Millisecond*3/4 {
		t.Fatalf("preload took %v; not parallel", d)
	}
}
