//go:build integration

// S5（议题 #18）20 节点端到端验收三场景：
//  1. TestIntegrationTwentyNodeCapacityAwareDistribution —— 归一化负载分布符合容量比；
//  2. TestIntegrationFaultTripHalfOpenProbeRecoveryChain —— 故障注入→熔断→half-open 探测→恢复全链路；
//  3. TestIntegrationPausedZeroTrafficAndAllMuted503 —— 下线节点零流量 + 全不可用 503。
//
// 全程经 /admin/metrics 校验调度观测字段（phase / affinity_hits / breaker_trips / in_flight）。
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admin"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/logging"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// ---- 共享 harness ----

// schedUpstream 是可控上游：可注入 500 故障、可选地把到达请求挂起（模拟在途容量占用），
// 到达时向 arrivals 信箱投递 ack（供发射端确认「槽位已登记」后再放行下一个候选竞争）。
type schedUpstream struct {
	srv      *httptest.Server
	hits     atomic.Int64
	fail     atomic.Bool
	hold     atomic.Bool
	release  atomic.Value // chan struct{}
	arrivals atomic.Value // chan chan struct{}（容量充足的无界信箱）
	gateOnce sync.Once
}

func newSchedUpstream(hold bool) *schedUpstream {
	u := &schedUpstream{}
	u.hold.Store(hold)
	u.release.Store(make(chan struct{}))
	u.arrivals.Store(make(chan chan struct{}, 8192))
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if u.hold.Load() {
			if ch, ok := u.arrivals.Load().(chan chan struct{}); ok {
				ch <- make(chan struct{})
			}
			select {
			case <-u.release.Load().(chan struct{}):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if u.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "injected upstream failure"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-sched", "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "ok"}}},
		})
	})
	u.srv = httptest.NewServer(mux)
	return u
}

// arrivalQueue 返回到达信箱本体（select 中用 <-u.arrivalQueue() 取 ack；
// 不要在函数内先接收——那会在 select 求值期阻塞消费）。
func (u *schedUpstream) arrivalQueue() chan chan struct{} {
	return u.arrivals.Load().(chan chan struct{})
}

// openGate 幂等放行所有被挂起的请求（此后新到的挂起等待也立即通过）。
func (u *schedUpstream) openGate() {
	u.gateOnce.Do(func() { close(u.release.Load().(chan struct{})) })
}

// close 先放闸再关服务，避免 httptest.Close 等待仍被挂起的处理器。
func (u *schedUpstream) close() {
	u.openGate()
	u.srv.Close()
}

type schedFixture struct {
	svc    *proxy.Service
	adminH *admin.Handler
	store  *auth.Store
	closeF []func()
}

func newSchedFixture(t *testing.T, mut func(*config.Config)) *schedFixture {
	t.Helper()
	tempDir := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 10,
			QuietPeriodSeconds:    1, // S5：压缩熔断冷却窗口供端到端验收
		},
		DataStore: config.DataStore{DBPath: filepath.Join(tempDir, "test.db")},
		AdminSession: config.AdminSessionConfig{CookieName: "admin_sid", Secret: "test-secret", TTLSeconds: 3600},
		Logging: config.LoggingConfig{
			Level:     "error",
			AccessLog: filepath.Join(tempDir, "access.log"),
			ErrorLog:  filepath.Join(tempDir, "error.log"),
		},
	}
	clients := []config.Client{{Name: "sched-e2e", AccessKey: "sched-token"}}
	if mut != nil {
		mut(cfg)
	}

	logManager, err := logging.Setup(logging.Config{
		Level: cfg.Logging.Level, AccessLogPath: cfg.Logging.AccessLog, ErrorLogPath: cfg.Logging.ErrorLog,
	})
	if err != nil {
		t.Fatalf("logging setup: %v", err)
	}
	f := &schedFixture{}
	f.closeF = append(f.closeF, func() { _ = logManager.Close() })
	appLogger := logManager.App()

	db, err := nosql.OpenDB(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	f.closeF = append(f.closeF, func() { _ = db.Close() })

	clientStore := nosql.NewClientStore(db)
	for _, c := range clients {
		if err := clientStore.Create(c); err != nil {
			t.Fatalf("seed client: %v", err)
		}
	}

	f.svc, err = proxy.NewService(cfg, appLogger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}
	f.store = auth.NewStore()
	if err := f.store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	manager := config.NewManager(filepath.Join(tempDir, "config.json"))
	manager.Replace(cfg)
	f.adminH = admin.NewHandler(manager, f.store, f.svc,
		nosql.NewAuditStore(db), nosql.NewUserStore(db), nosql.NewClientStore(db),
		nosql.NewTargetStore(db), nosql.NewModelCostStore(db), nosql.NewUsageStore(db),
		nil, nil, nil, nil, appLogger)
	t.Cleanup(func() {
		for _, c := range f.closeF {
			c()
		}
	})
	return f
}

type runtimeNode struct {
	Name           string  `json:"name"`
	InFlight       int64   `json:"in_flight"`
	MaxConcurrency int     `json:"max_concurrency"`
	Utilization    float64 `json:"utilization"`
	Paused         bool    `json:"paused"`
	Phase          string  `json:"phase"`
	AffinityHits   int64   `json:"affinity_hits"`
	BreakerTrips   int64   `json:"breaker_trips"`
}

func fetchRuntimeMap(t *testing.T, h *admin.Handler) map[string]runtimeNode {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin metrics: got %d", rec.Code)
	}
	var payload struct {
		TargetsRuntime []runtimeNode `json:"targets_runtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	m := make(map[string]runtimeNode, len(payload.TargetsRuntime))
	for _, rt := range payload.TargetsRuntime {
		m[rt.Name] = rt
	}
	return m
}

// waitForRuntime 轮询断言指标在 deadline 内满足条件（Mark 与响应写出存在窗口竞态）。
func waitForRuntime(t *testing.T, h *admin.Handler, name string, cond func(runtimeNode) bool, desc string) runtimeNode {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last runtimeNode
	for time.Now().Before(deadline) {
		last = fetchRuntimeMap(t, h)[name]
		if cond(last) {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("runtime condition not met for %q (%s): %+v", name, desc, last)
	return last
}

func doRequest(t *testing.T, svc *proxy.Service, store *auth.Store, clientIP, explicitTarget string) (int, string) {
	t.Helper()
	body := `{"model":"sched-e2e-model","messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", clientIP)
	req.RemoteAddr = clientIP + ":9" // 私网直连 ⇒ middleware 信任 XFF
	if explicitTarget != "" {
		req.Header.Set("X-Proxy-Target", explicitTarget)
	}
	principal, ok := store.Authenticate("sched-token")
	if !ok {
		t.Fatal("principal missing")
	}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// fireAsync 以独立身份发出一个并发请求（唯一 XFF+RemoteAddr 打散亲和，保证走 S2 负载调度
// 而非回粘），完成后把状态码写入 out 槽位。
func fireAsync(f *schedFixture, seq int, out []int32, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		body := `{"model":"sched-e2e-model","messages":[{"role":"user","content":"ping"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ip := fmt.Sprintf("10.%d.%d.%d", (seq>>16)%256, (seq>>8)%256, seq%256)
		req.Header.Set("X-Forwarded-For", ip)
		req.RemoteAddr = ip + ":9" // 私网直连 ⇒ middleware 信任 XFF
		p, _ := f.store.Authenticate("sched-token")
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rr := httptest.NewRecorder()
		f.svc.ServeHTTP(rr, req)
		resp := rr.Result()
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		out[seq] = int32(resp.StatusCode)
	}()
}

// ---- 场景①：20 节点归一化负载分布符合容量比（K=160 ≤ Σcap=248，全程不触硬上限拒绝）----

func TestIntegrationTwentyNodeCapacityAwareDistribution(t *testing.T) {
	caps := []int{2, 4, 8, 16, 32} // ×4 成员 = 20 节点
	nodes := make([]*schedUpstream, 0, 20)
	names := make([]string, 0, 20)
	nameToCap := map[string]int{}
	f := newSchedFixture(t, func(cfg *config.Config) {
		idx := 0
		for _, c := range caps {
			for rep := 0; rep < 4; rep++ {
				u := newSchedUpstream(true)
				name := fmt.Sprintf("node-%02d", idx)
				nodes = append(nodes, u)
				names = append(names, name)
				nameToCap[name] = c
				cfg.Targets = append(cfg.Targets, config.Target{
					Name: name, EndpointType: "openai",
					Endpoint: u.srv.URL, APIKey: fmt.Sprintf("k-%d", idx),
					MaxConcurrency: c,
				})
				idx++
			}
		}
	})
	defer func() {
		for _, u := range nodes {
			u.close()
		}
	}()

	const K = 160

	// 放置阶段：串行化贪水决策——第 i 个请求真正落到某上游（槽位已计入原子在途数）
	// 之后才放行第 i+1 个候选竞争，使最终分布等价于纯串行最小负载调度。
	responses := make([]int32, K)
	var wg sync.WaitGroup
	totalHits := func() int64 {
		var s int64
		for _, u := range nodes {
			s += u.hits.Load()
		}
		return s
	}
	for i := 0; i < K; i++ {
		before := totalHits()
		fireAsync(f, i, responses, &wg)
		deadline := time.Now().Add(5 * time.Second)
		for totalHits() <= before {
			if time.Now().After(deadline) {
				t.Fatalf("placement #%d never landed upstream", i)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// 全部放置完成：开闸放行在途请求，回收响应。
	for _, u := range nodes {
		u.openGate()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("in-flight requests did not drain after gate opened")
	}

	statuses := map[int32]int{}
	for _, c := range responses {
		statuses[c]++
	}
	if statuses[200] != K {
		t.Fatalf("expected all %d requests to succeed, census=%+v", K, statuses)
	}

	// 分布断言（并列由加权轮询打散 → 按容量类聚合对照期望 K·classCap/Σcap；
	// 容差覆盖跨类边界水位抖动）。
	classHits := map[int]int{}
	nodeHits := map[string]int64{}
	for ni, u := range nodes {
		hits := u.hits.Load()
		nodeHits[names[ni]] = hits
		classHits[nameToCap[names[ni]]] += int(hits)
	}
	for _, c := range caps {
		exp := float64(K) * float64(c*4) / 248.0
		t.Logf("容量类 cap=%d 命中=%d 期望≈%.1f（归一化负载均摊）", c, classHits[c], exp)
	}
	var diagNames []string
	for _, n := range names {
		diagNames = append(diagNames, fmt.Sprintf("%s=%d", n, nodeHits[n]))
	}
	t.Logf("逐节点命中: %v", diagNames)
	prevSum := -1
	for ci, c := range caps {
		exp := float64(K) * float64(c*4) / 248.0
		got := float64(classHits[c])
		if diff := got - exp; diff > 0.35*exp+3 || diff < -(0.35*exp+3) {
			t.Fatalf("capacity=%d distribution off: got %.0f want ~%.1f", c, got, exp)
		}
		if got <= 0 {
			t.Fatalf("capacity=%d class received zero traffic", c)
		}
		if prevSum >= 0 && classHits[c] <= prevSum {
			t.Fatalf("monotonicity broken at class %d: %d ≤ previous %d", c, classHits[c], prevSum)
		}
		prevSum = classHits[c]
		_ = ci
	}
	for name, hits := range nodeHits {
		if int(hits) > nameToCap[name] {
			t.Fatalf("node %s served %d > hard cap %d", name, hits, nameToCap[name])
		}
	}

	// 收敛断言：闸门放开后在途归零、相位回 ready（S5 观测面自证释放链路无泄漏）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rt := fetchRuntimeMap(t, f.adminH)
		okPhase, drained := 0, 0
		for _, name := range names {
			r, ok := rt[name]
			if !ok {
				continue
			}
			if r.Phase == "ready" {
				okPhase++
			}
			if r.InFlight == 0 {
				drained++
			}
		}
		if okPhase == len(nodes) && drained == len(nodes) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("post-release runtime never converged to ready/drained state")
}

// ---- 场景②：故障注入→熔断→冷却→half-open 探测失败延长→探测成功恢复 全链路 ----

func TestIntegrationFaultTripHalfOpenProbeRecoveryChain(t *testing.T) {
	var flaky, healthy *schedUpstream
	f := newSchedFixture(t, func(cfg *config.Config) {
		flaky = newSchedUpstream(true) // 具备挂起能力，初始直通（探针阶段临时启用）
		flaky.hold.Store(false)
		healthy = newSchedUpstream(false)
		cfg.Targets = []config.Target{
			{Name: "flaky-node", EndpointType: "openai",
				Endpoint: flaky.srv.URL, APIKey: "kf"},
			{Name: "healthy-paused", EndpointType: "openai",
				Endpoint: healthy.srv.URL, APIKey: "kh", Paused: true},
		}
	})
	defer flaky.close()
	defer healthy.close()

	flaky.fail.Store(true) // 故障注入：上游恒 500

	// ① 首请求 → 上游 500 透传（v3：不做同 target 重试），单次失败即跨阈值熔断。
	code, _ := doRequest(t, f.svc, f.store, "10.7.0.1", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("first request should pass through upstream 500, got %d (flaky hits=%d healthy hits=%d)",
			code, flaky.hits.Load(), healthy.hits.Load())
	}
	waitForRuntime(t, f.adminH, "flaky-node", func(r runtimeNode) bool {
		return r.Phase == "muted" && r.BreakerTrips == 1
	}, "after first failure")

	// ② 冷却期内的后续自动选择 → 503 all targets muted until X（绝不吸入流量）。
	code, body := doRequest(t, f.svc, f.store, "10.7.0.2", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "all targets muted until") {
		t.Fatalf("during cooldown expected 503 muted message, got %d %s", code, body)
	}
	if flaky.hits.Load() != 1 {
		t.Fatalf("cooldown window must not schedule traffic, flaky hits=%d", flaky.hits.Load())
	}
	// paused 对照节点显式点名同样被拒（下线优先于一切状态）。
	code, body = doRequest(t, f.svc, f.store, "10.7.0.2", "healthy-paused")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "requested target paused") {
		t.Fatalf("explicit paused target expected 503 requested-target-paused, got %d %s", code, body)
	}
	if healthy.hits.Load() != 0 {
		t.Fatalf("paused node received traffic: hits=%d", healthy.hits.Load())
	}

	// ③ 静默期过期进入 half-open：下一请求成为独占探针；上游仍故障 → 探测失败延长冷却
	//    （基线 1s ×2），breaker_trips 叠加为 2，且被挂起的探针可在途观测 phase=half-open。
	time.Sleep(1150 * time.Millisecond)
	flaky.hold.Store(true) // 让探针停在在途，确定性抓取 half-open 快照
	probeDone := make(chan int, 1)
	go func() { c, _ := doRequest(t, f.svc, f.store, "10.7.0.3", ""); probeDone <- c }()
	select {
	case ack := <-flaky.arrivalQueue():
		close(ack)
	case c := <-probeDone:
		t.Fatalf("probe rejected before reaching upstream, status=%d hits=%d", c, flaky.hits.Load())
	case <-time.After(5 * time.Second):
		rtNow := fetchRuntimeMap(t, f.adminH)
		t.Fatalf("probe stalled: hits=%d hold=%v runtime[flaky]=%+v runtime[healthy]=%+v",
			flaky.hits.Load(), flaky.hold.Load(), rtNow["flaky-node"], rtNow["healthy-paused"])
	}
	waitForRuntime(t, f.adminH, "flaky-node", func(r runtimeNode) bool {
		return r.Phase == "half-open" && r.InFlight == 1
	}, "probe mid-flight")
	flaky.openGate() // 探针收到 500 ⇒ MarkFailure 延长冷却
	if code := <-probeDone; code != http.StatusInternalServerError {
		t.Fatalf("failed probe should pass through 500, got %d", code)
	}
	waitForRuntime(t, f.adminH, "flaky-node", func(r runtimeNode) bool {
		return r.Phase == "muted" && r.BreakerTrips == 2
	}, "after failed probe extends cooldown")

	// ④ 延长冷却期内依旧零流量。
	code, _ = doRequest(t, f.svc, f.store, "10.7.0.4", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("extended cooldown must reject scheduling, got %d", code)
	}

	// ⑤ 再次到期 → 探针成功 → 恢复 ready，后续全量接客。
	time.Sleep(2100 * time.Millisecond)
	flaky.fail.Store(false)
	code, _ = doRequest(t, f.svc, f.store, "10.7.0.5", "")
	if code != http.StatusOK {
		t.Fatalf("successful probe should recover with 200, got %d", code)
	}
	waitForRuntime(t, f.adminH, "flaky-node", func(r runtimeNode) bool {
		return r.Phase == "ready"
	}, "after successful probe recovery")
	// 同一客户端（同 IP+model）三连发：首发建立粘连，后续命中回粘 ⇒ affinity_hits ≥ 2。
	for i := 0; i < 3; i++ {
		code, _ = doRequest(t, f.svc, f.store, "10.7.2.7", "")
		if code != http.StatusOK {
			t.Fatalf("post-recovery request #%d got %d", i, code)
		}
	}
	final := waitForRuntime(t, f.adminH, "flaky-node", func(r runtimeNode) bool {
		return r.Phase == "ready" && r.BreakerTrips == 2 && r.AffinityHits >= 2
	}, "final steady state keeps trips and counts affinity")
	if final.AffinityHits < 2 {
		t.Fatalf("affinity_hits=%d, want ≥2 after repeated same-client successes", final.AffinityHits)
	}
}

// ---- 场景③：下线节点零流量 + 全部健康节点故障后 503 ----

func TestIntegrationPausedZeroTrafficAndAllMuted503(t *testing.T) {
	var upA, upB, offline *schedUpstream
	f := newSchedFixture(t, func(cfg *config.Config) {
		upA = newSchedUpstream(false)
		upB = newSchedUpstream(false)
		offline = newSchedUpstream(false)
		cfg.Targets = []config.Target{
			{Name: "ok-a", EndpointType: "openai", Endpoint: upA.srv.URL, APIKey: "ka"},
			{Name: "ok-b", EndpointType: "openai", Endpoint: upB.srv.URL, APIKey: "kb"},
			{Name: "off-node", EndpointType: "openai", Endpoint: offline.srv.URL, APIKey: "ko", Paused: true},
		}
	})
	defer upA.close()
	defer upB.close()
	defer offline.close()

	// 显式点名 paused 节点 → 503 requested target paused，且零流量。
	code, body := doRequest(t, f.svc, f.store, "10.9.0.1", "off-node")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "requested target paused") {
		t.Fatalf("explicit paused expected 503 requested-target-paused, got %d %s", code, body)
	}
	offlineHitsBefore := offline.hits.Load()

	// 注入故障：随后 6 连发会把两台健康节点先后打熔断（单次失败即静默）；
	// off-node 必须始终零流量。
	upA.fail.Store(true)
	upB.fail.Store(true)
	saw503Muted := false
	for i := 0; i < 8; i++ {
		code, body := doRequest(t, f.svc, f.store, fmt.Sprintf("10.9.1.%d", i), "")
		if code == http.StatusServiceUnavailable && strings.Contains(body, "all targets muted until") {
			saw503Muted = true
		} else if code != http.StatusInternalServerError {
			t.Fatalf("fault injection loop #%d unexpected status %d: %s", i, code, body)
		}
	}
	if !saw503Muted {
		t.Fatal("never observed 503 'all targets muted until' with every healthy node muted")
	}
	if offline.hits.Load() != offlineHitsBefore {
		t.Fatalf("offline node served traffic during cascading failure: %d -> %d",
			offlineHitsBefore, offline.hits.Load())
	}
	// 最终态观测：两个故障节点 muted、off-node 恒 offline。
	rt := fetchRuntimeMap(t, f.adminH)
	if rt["ok-a"].Phase != "muted" || rt["ok-b"].Phase != "muted" {
		t.Fatalf("both faulted nodes should be muted: %+v / %+v", rt["ok-a"], rt["ok-b"])
	}
	if rt["off-node"].Phase != "offline" {
		t.Fatalf("paused node phase=%q, want offline", rt["off-node"].Phase)
	}
}
