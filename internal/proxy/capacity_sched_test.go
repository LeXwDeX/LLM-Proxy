package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// newCapacityTestService 构建仅含给定 targets 的代理服务（openai 类型、单 key），
// 供容量调度单元测试直接驱动选路逻辑。
func newCapacityTestService(t *testing.T, mutators ...func(*config.Target)) *Service {
	t.Helper()
	targets := []config.Target{
		{Name: "cap-a", EndpointType: "openai", Endpoint: "http://127.0.0.1:1", APIKey: "k1"},
		{Name: "cap-b", EndpointType: "openai", Endpoint: "http://127.0.0.1:2", APIKey: "k2"},
		{Name: "cap-u", EndpointType: "openai", Endpoint: "http://127.0.0.1:3", APIKey: "k3"},
	}
	for _, m := range mutators {
		for i := range targets {
			m(&targets[i])
		}
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
	return svc
}

func capState(t *testing.T, svc *Service, name string) *targetState {
	t.Helper()
	st, ok := svc.targetByName(name)
	if !ok || st == nil {
		t.Fatalf("target state %q missing", name)
	}
	return st
}

// fill 将节点在途数填到指定值（模拟上游占用的并发请求）。
func fill(t *testing.T, st *targetState, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !st.acquireSlot() {
			t.Fatalf("acquireSlot failed prematurely for %q at %d", st.Target().Name, i)
		}
	}
}

// pick 自动路径选路一次（allowed 全开、无 attempted、无模型过滤歧义）。
func pick(t *testing.T, svc *Service) (*targetState, selectionKind, error) {
	t.Helper()
	p := &auth.Principal{Name: fmt.Sprintf("u-%d", time.Now().UnixNano())}
	now := time.Now()
	state, _, kind, err := svc.selectTarget(p, "10.0.0.1", "", "", nil, nil, "", "/v1/chat/completions", now)
	return state, kind, err
}

// TestCapacityHardCapExcludedAndBypassed 验收②：达硬上限的节点不再被选为新请求候选，
// 流量绕行其他节点；全部满载时拒绝新请求而非强塞。
func TestCapacityHardCapExcludedAndBypassed(t *testing.T) {
	svc := newCapacityTestService(t, func(tc *config.Target) {
		if tc.Name == "cap-u" {
			tc.Paused = true
		}
		tc.MaxConcurrency = 2
	})
	a := capState(t, svc, "cap-a")
	b := capState(t, svc, "cap-b")

	fill(t, a, 2) // cap-a 满
	for i := 0; i < 20; i++ {
		st, _, err := pick(t, svc)
		if err != nil {
			t.Fatalf("select #%d: unexpected error %v", i, err)
		}
		if got := strings.ToLower(st.Target().Name); got != "cap-b" {
			t.Fatalf("selection #%d went to saturated cap-a: got %q", i, got)
		}
	}

	// cap-b 也满载、cap-a 释放一个槽位 ⇒ 唯一合法候选是刚恢复资格的 cap-a。
	a.releaseSlot()
	fill(t, b, 2)
	st, _, err := pick(t, svc)
	if err != nil {
		t.Fatalf("select after release: %v", err)
	}
	if got := strings.ToLower(st.Target().Name); got != "cap-a" {
		t.Fatalf("expected exclusively-eligible cap-a, got %q", got)
	}

	// 双双满载：显式拒绝（503，消息点名容量）。
	fill(t, a, 1)
	_, _, err = pick(t, svc)
	if err == nil {
		t.Fatal("expected capacity refusal when all capped targets are full")
	}
	var selErr *selectionError
	if !errors.As(err, &selErr) {
		t.Fatalf("expected selectionError, got %T: %v", err, err)
	}
	if selErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", selErr.Status, selErr.Message)
	}
	if !strings.Contains(selErr.Message, "capacity") {
		t.Fatalf("expected capacity-related message, got %q", selErr.Message)
	}
}

// TestNormalizedLoadLowestWins 验收①方向性：归一化负载更低者必然胜出，
// 且负载反转后选择随之反转。
func TestNormalizedLoadLowestWins(t *testing.T) {
	svc := newCapacityTestService(t, func(tc *config.Target) {
		switch tc.Name {
		case "cap-a":
			tc.MaxConcurrency = 4
		case "cap-b":
			tc.MaxConcurrency = 2
		default:
			tc.Paused = true // 第三节点退出本轮比较
		}
	})
	a := capState(t, svc, "cap-a")
	b := capState(t, svc, "cap-b")

	// A: 3/4=0.75, B: 1/2=0.5 → 必选 B
	fill(t, a, 3)
	fill(t, b, 1)
	for i := 0; i < 10; i++ {
		st, _, err := pick(t, svc)
		if err != nil {
			t.Fatalf("select #%d: %v", i, err)
		}
		if got := strings.ToLower(st.Target().Name); got != "cap-b" {
			t.Fatalf("#%d expected lower-load cap-b, got %q (loads %.2f vs %.2f)", i, got, a.normalizedLoad(), b.normalizedLoad())
		}
		// 选路不改变在途计数（计数只发生在转发前登记）。
		if st.inFlightCount() != 1 {
			t.Fatalf("selection must not mutate in-flight, got %d", st.inFlightCount())
		}
	}

	// 反转：A 降到 1/4=0.25 → 必选 A
	a.releaseSlot()
	a.releaseSlot()
	for i := 0; i < 10; i++ {
		st, _, err := pick(t, svc)
		if err != nil {
			t.Fatalf("select #%d after flip: %v", i, err)
		}
		if got := strings.ToLower(st.Target().Name); got != "cap-a" {
			t.Fatalf("#%d expected flipped lower-load cap-a, got %q", i, got)
		}
	}
}

// TestNormalizedLoadProportionalSplitTwoToOne 验收①稳态分流：容量 4:2 的两节点
// 在维持固定并发的持续流下，归一化负载被打平到同一水位 ⇒ 吸收的请求数趋近 2:1。
// 多轮独立试验取中位数消除并列随机打散的抖动。
func TestNormalizedLoadProportionalSplitTwoToOne(t *testing.T) {
	svc := newCapacityTestService(t, func(tc *config.Target) {
		switch tc.Name {
		case "cap-a":
			tc.MaxConcurrency = 4
		case "cap-b":
			tc.MaxConcurrency = 2
		default:
			tc.Paused = true
		}
	})
	a := capState(t, svc, "cap-a")
	b := capState(t, svc, "cap-b")

	const (
		trials      = 21
		admitsTotal = 400
		outstanding = 3 // 固定并发管线：低于任何单节点容量，杜绝饱和干扰纯分流观测
	)
	ratios := make([]float64, 0, trials)
	for trial := 0; trial < trials; trial++ {
		a.inFlight.Store(0)
		b.inFlight.Store(0)
		countA, countB := 0, 0
		releaseQ := make([]*targetState, 0, outstanding)
		for i := 0; i < admitsTotal; i++ {
			if len(releaseQ) >= outstanding {
				releaseQ[0].releaseSlot()
				releaseQ = releaseQ[1:]
			}
			st, _, err := pick(t, svc)
			if err != nil {
				t.Fatalf("trial %d admit %d: %v", trial, i, err)
			}
			switch strings.ToLower(st.Target().Name) {
			case "cap-a":
				countA++
			case "cap-b":
				countB++
			}
			if !st.acquireSlot() {
				t.Fatalf("trial %d admit %d: freshly selected %q refused slot", trial, i, st.Target().Name)
			}
			releaseQ = append(releaseQ, st)
		}
		for _, st := range releaseQ {
			st.releaseSlot()
		}
		if countB > 0 {
			ratios = append(ratios, float64(countA)/float64(countB))
		}
	}
	sortFloats(ratios)
	median := ratios[len(ratios)/2]
	if median < 1.6 || median > 2.5 {
		t.Fatalf("median split ratio = %.2f, want ≈2:1 within [1.6,2.5] (spread %.2f..%.2f)", median, ratios[0], ratios[len(ratios)-1])
	}
}

func sortFloats(xs []float64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// TestUncappedTargetStaysEligibleWhenCappedPeersFull：全部满载但存在未配置容量的
// 节点时，必须继续由该节点服务（未设容量=不设限），不能误报全忙。
func TestUncappedTargetStaysEligibleWhenCappedPeersFull(t *testing.T) {
	svc := newCapacityTestService(t, func(tc *config.Target) {
		if tc.Name == "cap-a" || tc.Name == "cap-b" {
			tc.MaxConcurrency = 1
		}
	})
	fill(t, capState(t, svc, "cap-a"), 1)
	fill(t, capState(t, svc, "cap-b"), 1)
	u := capState(t, svc, "cap-u")
	u.inFlight.Store(9999)

	for i := 0; i < 10; i++ {
		st, _, err := pick(t, svc)
		if err != nil {
			t.Fatalf("select #%d: %v", i, err)
		}
		if got := strings.ToLower(st.Target().Name); got != "cap-u" {
			t.Fatalf("#%d expected uncapped cap-u to serve, got %q", i, got)
		}
	}
}

// TestForwardRequestNodeInFlightLifecycle 验证 forwardRequest 的节点在途计数：
// 成功路径随响应体 Close 释放（移交式）；网络错误路径 defer 兜底释放；
// 选择-占用竞态下满载直接返回可 failover 的 503 且不推进计数。
func TestForwardRequestNodeInFlightLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	svc := newCapacityTestService(t, func(tc *config.Target) {
		tc.Endpoint = upstream.URL
		tc.ModelMappings = []config.ModelMapping{{Upstream: "gpt-test"}}
		tc.MaxConcurrency = 1
	})
	st := capState(t, svc, "cap-a")

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
		return req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Name: "tester"}))
	}

	// 成功：登记→移交 Body.Close 释放。
	resp, cancel, _, ferr := svc.forwardRequest(newReq(), st, []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`), "", nil)
	if ferr != nil {
		t.Fatalf("forwardRequest success path: %v", ferr)
	}
	if got := st.inFlightCount(); got != 1 {
		t.Fatalf("in-flight during streaming = %d, want 1", got)
	}
	resp.Body.Close()
	cancel()
	if got := st.inFlightCount(); got != 0 {
		t.Fatalf("in-flight after body close = %d, want 0", got)
	}

	// 网络错误：defer 兜底释放，绝不泄漏计数。
	deadSvc := newCapacityTestService(t, func(tc *config.Target) { tc.MaxConcurrency = 1 })
	dst := capState(t, deadSvc, "cap-a")
	_, _, _, ferr = deadSvc.forwardRequest(newReq(), dst, []byte(`{}`), "", nil)
	if ferr == nil {
		t.Fatal("expected network error against dead endpoint")
	}
	if got := dst.inFlightCount(); got != 0 {
		t.Fatalf("in-flight leaked after network error = %d", got)
	}

	// 满载竞态：acquire 失败 → 503/retryable/skipMute，计数不变。
	fill(t, st, 1)
	resp2, cancel2, idx2, ferr2 := svc.forwardRequest(newReq(), st, []byte(`{}`), "", nil)
	if ferr2 == nil {
		t.Fatal("expected max-concurrency rejection")
	}
	if resp2 != nil || cancel2 != nil || idx2 != -1 {
		t.Fatalf("unexpected artifacts on rejection: resp=%v cancel=%v idx=%d", resp2, cancel2, idx2)
	}
	var fae *forwardAttemptError
	if !errors.As(ferr2, &fae) {
		t.Fatalf("expected forwardAttemptError, got %T", ferr2)
	}
	if fae.status != http.StatusServiceUnavailable || !fae.retryable || !fae.skipMute {
		t.Fatalf("rejection flags wrong: status=%d retryable=%v skipMute=%v", fae.status, fae.retryable, fae.skipMute)
	}
	if got := st.inFlightCount(); got != 1 {
		t.Fatalf("saturated node count drifted: %d", got)
	}
}

// TestConcurrentAcquireNeverExceedsHardCap：并发争抢下 acquireSlot 恰好放行
// 容量数个槽位，绝不越界。
func TestConcurrentAcquireNeverExceedsHardCap(t *testing.T) {
	svc := newCapacityTestService(t, func(tc *config.Target) { tc.MaxConcurrency = 3 })
	st := capState(t, svc, "cap-a")

	const contenders = 64
	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if st.acquireSlot() {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != 3 {
		t.Fatalf("admitted = %d, want exactly 3", admitted)
	}
	if got := st.inFlightCount(); got != 3 {
		t.Fatalf("in-flight = %d, want 3", got)
	}
}

func TestTargetStatusExposesCapacityFields(t *testing.T) {
	svc := newCapacityTestService(t, func(tc *config.Target) {
		switch tc.Name {
		case "cap-a":
			tc.MaxConcurrency = 4
		case "cap-b":
			tc.MaxConcurrency = 2
		}
	})
	fill(t, capState(t, svc, "cap-a"), 3)
	fill(t, capState(t, svc, "cap-b"), 2)

	statuses := svc.TargetStatuses(time.Now())
	byName := map[string]TargetStatus{}
	for _, s := range statuses {
		byName[strings.ToLower(s.Name)] = s
	}
	a := byName["cap-a"]
	if a.Capacity != 4 || a.InFlight != 3 {
		t.Fatalf("cap-a status = %+v, want capacity 4 in-flight 3", a)
	}
	if diff := a.Utilization - 0.75; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cap-a utilization = %f, want 0.75", a.Utilization)
	}
	bStat := byName["cap-b"]
	if bStat.InFlight != 2 || bStat.Capacity != 2 || bStat.Utilization != 1 {
		t.Fatalf("cap-b status = %+v, want saturated 2/2 @1.0", bStat)
	}
	uStat := byName["cap-u"]
	if uStat.Capacity != 0 || uStat.Utilization != 0 {
		t.Fatalf("uncapped cap-u status = %+v, want capacity 0 utilization 0", uStat)
	}
}
