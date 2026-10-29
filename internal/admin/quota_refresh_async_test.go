package admin

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

// slowQuota 模拟「刷新很慢」的额度管理器：ScheduleRefresh 自身也故意拖慢，
// 若 handler 在持有 configMu 时调用它，并发的目标列表读取就会被阻塞。
type slowQuota struct {
	h        *Handler
	delay    time.Duration
	entered  chan struct{}
	mu       sync.Mutex
	scopes   []quota.RefreshScope
	lockHeld []bool
}

func (q *slowQuota) Status(string) quota.QuotaStatus { return quota.QuotaStatus{} }
func (q *slowQuota) Evaluate(string)                 {}
func (q *slowQuota) ScheduleRefresh(scope quota.RefreshScope) {
	held := !q.h.configMu.TryLock()
	if !held {
		q.h.configMu.Unlock()
	}
	q.mu.Lock()
	q.scopes = append(q.scopes, scope)
	q.lockHeld = append(q.lockHeld, held)
	q.mu.Unlock()
	select {
	case q.entered <- struct{}{}:
	default:
	}
	time.Sleep(q.delay)
}

func (q *slowQuota) snapshot() ([]quota.RefreshScope, []bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]quota.RefreshScope(nil), q.scopes...), append([]bool(nil), q.lockHeld...)
}

func TestDeleteTargetDoesNotHoldConfigLockWhileRefreshing(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	q := &slowQuota{h: e.h, delay: 600 * time.Millisecond, entered: make(chan struct{}, 4)}
	e.h.quotaManager = q

	done := make(chan int, 1)
	go func() { done <- e.do(http.MethodDelete, "targets/target-b", "", nil).Code }()
	select {
	case <-q.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh was never scheduled")
	}
	start := time.Now()
	rec := e.do(http.MethodGet, "targets", "", nil)
	if d := time.Since(start); d > 100*time.Millisecond || rec.Code != http.StatusOK {
		t.Fatalf("GET /targets during slow refresh: code=%d took %v, want <100ms", rec.Code, d)
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	scopes, held := q.snapshot()
	if len(scopes) != 1 || len(scopes[0].Targets) != 1 || scopes[0].Targets[0] != "target-b" || scopes[0].All {
		t.Fatalf("delete target must schedule a scoped refresh, got %+v", scopes)
	}
	if held[0] {
		t.Fatal("configMu was held while scheduling the quota refresh")
	}
	// 审计在释放锁后仍然同步写入。
	events, err := e.stores.auditStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		found = found || (ev.Action == "delete_target" && ev.Object == "target-b")
	}
	if !found {
		t.Fatalf("delete_target audit event missing: %+v", events)
	}
}

func TestAdminWritesScheduleScopedRefreshInsteadOfSyncRefreshAll(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	q := &slowQuota{h: e.h, entered: make(chan struct{}, 16)}
	e.h.quotaManager = q

	for _, step := range []struct {
		method, path string
		body         any
		want         quota.RefreshScope
	}{
		{http.MethodPost, "model-groups", config.ModelGroup{Name: "g1", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "target-a"}}}, quota.RefreshScope{Groups: []string{"g1"}}},
		{http.MethodPost, "job-levels", config.JobLevel{Name: "L5"}, quota.RefreshScope{}},
		{http.MethodPut, "job-levels/L5", config.JobLevel{Name: "L5"}, quota.RefreshScope{JobLevels: []string{"L5", "L5"}}},
		{http.MethodDelete, "model-groups/g1", nil, quota.RefreshScope{Groups: []string{"g1"}}},
		{http.MethodPut, "model-costs/qwen-max", map[string]any{"input_per_1m_tokens": 1, "output_per_1m_tokens": 2}, quota.RefreshScope{All: true}},
	} {
		before, _ := q.snapshot()
		rec := e.do(step.method, step.path, "", step.body)
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", step.method, step.path, rec.Code, rec.Body.String())
		}
		after, held := q.snapshot()
		if step.want.All == false && len(step.want.Groups) == 0 && len(step.want.JobLevels) == 0 {
			if len(after) != len(before) {
				t.Fatalf("%s %s must not schedule a refresh", step.method, step.path)
			}
			continue
		}
		if len(after) != len(before)+1 {
			t.Fatalf("%s %s scheduled %d refreshes, want 1", step.method, step.path, len(after)-len(before))
		}
		got := after[len(after)-1]
		if got.All != step.want.All || len(got.Groups) != len(step.want.Groups) || len(got.JobLevels) != len(step.want.JobLevels) {
			t.Fatalf("%s %s scope = %+v, want %+v", step.method, step.path, got, step.want)
		}
		if held[len(held)-1] {
			t.Fatalf("%s %s scheduled while holding configMu", step.method, step.path)
		}
	}
}

// 目标列表的数据库读取（ETag、密钥 ID 密钥）发生在获取 configMu 之前。
func TestListTargetsReadsStoreBeforeTakingConfigLock(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	loaded := make(chan struct{})
	e.h.listETagsLoadedHook = func() { close(loaded) }
	e.h.configMu.Lock() // 模拟写操作正持有 configMu
	done := make(chan int, 1)
	go func() { done <- e.do(http.MethodGet, "targets", "", nil).Code }()
	select {
	case <-loaded:
	case <-time.After(2 * time.Second):
		e.h.configMu.Unlock()
		t.Fatal("ETags were not read until configMu was acquired")
	}
	select {
	case <-done:
		t.Fatal("list returned while configMu was held")
	case <-time.After(50 * time.Millisecond):
	}
	e.h.configMu.Unlock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
}
