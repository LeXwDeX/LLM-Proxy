package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
)

// 审计写入发生在释放 configMu 之后：慢审计存储不得阻塞并发的目标列表读取。
func TestAuditIsWrittenOutsideConfigLock(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	var heldDuringWrite atomic.Bool
	entered := make(chan struct{}, 1)
	e.h.auditWriteHook = func(nosql.AuditEvent) {
		if !e.h.configMu.TryLock() {
			heldDuringWrite.Store(true)
		} else {
			e.h.configMu.Unlock()
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		time.Sleep(400 * time.Millisecond) // 慢审计存储
	}
	done := make(chan int, 1)
	go func() { done <- e.do(http.MethodPut, "targets/target-b/pause", "", nil).Code }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("audit was never written")
	}
	start := time.Now()
	rec := e.do(http.MethodGet, "targets", "", nil)
	if d := time.Since(start); d > 150*time.Millisecond || rec.Code != http.StatusOK {
		t.Fatalf("GET /targets during slow audit: code=%d took %v", rec.Code, d)
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("pause: %d", code)
	}
	if heldDuringWrite.Load() {
		t.Fatal("audit was written while holding configMu")
	}
}

// 审计时间戳在 configMu 内取得：提交顺序即审计顺序，即使前一个事务的审计写入更慢。
func TestAuditOrderMatchesCommitOrder(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	firstWriting := make(chan struct{})
	var once sync.Once
	e.h.auditDeferredHook = func(action string) {
		if action == "first" {
			once.Do(func() { close(firstWriting) })
			time.Sleep(200 * time.Millisecond) // 前一事务的审计写入被拖慢到后一事务之后
		}
	}
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		txn := e.h.lockConfig(r)
		txn.audit("first", "o", "success", "")
		txn.Unlock() // 释放锁后审计写入被拖慢
	}()
	<-firstWriting
	txn := e.h.lockConfig(r) // 前一事务已提交并释放锁
	txn.audit("second", "o", "success", "")
	txn.Unlock() // 审计立即写完
	wg.Wait()
	events, err := e.stores.auditStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, ev := range events { // List 为倒序（新到旧）
		if ev.Action == "first" || ev.Action == "second" {
			order = append(order, ev.Action)
		}
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("audit order (newest first) = %v, want [second first]", order)
	}
}

// 目标写入提交后，锁外预先重建计价缓存：API 路径的首次计价不再回读库。
func TestCostCacheIsWarmedAfterTargetCommit(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	var loads atomic.Int32
	e.h.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		loads.Add(1)
		return nil, nil
	}, nil)
	if rec := e.do(http.MethodPut, "targets/target-b/pause", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	after := loads.Load()
	if after == 0 {
		t.Fatal("cost cache was not rebuilt after the commit")
	}
	if _, err := e.h.costCache.GetChecked(); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != after {
		t.Fatalf("first API-path pricing after commit hit the store (%d -> %d loads)", after, loads.Load())
	}
}

// 写响应带 quota_refresh，进度可由 /metrics 回读。
func TestWriteResponsesAnnounceQuotaRefreshAndMetricsReportsIt(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	q := &slowQuota{h: e.h, entered: make(chan struct{}, 8)}
	e.h.quotaManager = q
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{http.MethodDelete, "targets/target-b", nil},
		{http.MethodPut, "model-costs/qwen-max", map[string]any{"input_per_1m_tokens": 1, "output_per_1m_tokens": 2}},
	} {
		rec := e.do(step.method, step.path, "", step.body)
		if rec.Code != http.StatusOK || !jsonField(t, rec.Body.Bytes(), "quota_refresh", "pending") {
			t.Fatalf("%s %s: %d %s (want quota_refresh=pending)", step.method, step.path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"quota_refresh"`, `"config_generation"`, `"pending"`) {
		t.Fatalf("metrics lacks quota_refresh status: %d %s", rec.Code, rec.Body.String())
	}
}

func jsonField(t *testing.T, body []byte, key, want string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("response is not a JSON object: %v: %s", err, body)
	}
	got, _ := m[key].(string)
	return got == want
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
