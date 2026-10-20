package admin

// pool_acceptance_test.go — Issue #14 S1「部署池最小闭环」端到端验收测试。
//
// 全链路走真实公共实现：池 / 目标 / 客户端全部通过 admin CRUD API 创建
// （bbolt 持久化 + applyConfigRuntime 热传播），代理请求直接打到真实
// proxy.Service，不复制任何生产逻辑。
//
// 验收切片不新增单测语义（production / integration 切片各自守护），
// 这里只覆盖四条验收主线：
//  1. 2 节点池请求轮询，响应头 X-Proxy-Target=节点名、X-Proxy-Pool=池名；
//  2. 客户端仅授权池（allowed_pools）即可访问成员，池外目标 403；
//  3. 池暂停后成员等效暂停（显式选择 503；自动选择与逐个单独暂停成员
//     行为一致），恢复后回归；
//  4. 同规格无池 Target 行为零回归（旧客户端语义、响应头均不变）；
//  5. 池删除：有成员 409，摘除成员后可删，删后成员继续独立服务。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

const (
	accPoolName  = "pool-alpha"
	accPoolModel = "acc-chat-model"
	accSoloModel = "acc-solo-model"
)

// poolAcceptanceWorld 聚合一次验收测试所需的全部运行时对象。
type poolAcceptanceWorld struct {
	handler   http.Handler
	service   *proxy.Service
	authStore *auth.Store

	upA          *httptest.Server
	upB          *httptest.Server
	upStandalone *httptest.Server

	hitsA          atomic.Int64
	hitsB          atomic.Int64
	hitsStandalone atomic.Int64
}

// newAccUpstream 启动一个返回固定 usage 的 mock 上游，并统计命中次数。
func newAccUpstream(t *testing.T, model string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"model":%q,"usage":{"prompt_tokens":10,"completion_tokens":5}}`, model)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setupPoolAcceptanceWorld 按既有 handler_test 惯例搭建 admin handler +
// proxy service（配置文件初始不含任何目标，全部经 admin API 创建）。
func setupPoolAcceptanceWorld(t *testing.T) *poolAcceptanceWorld {
	t.Helper()
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 0, nil)
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	authStore := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}

	w := &poolAcceptanceWorld{
		handler: NewHandler(manager, authStore, service, stores.auditStore, stores.userStore,
			stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore,
			nil, nil, nil, nil, logger),
		service:   service,
		authStore: authStore,
	}
	w.upA = newAccUpstream(t, accPoolModel, &w.hitsA)
	w.upB = newAccUpstream(t, accPoolModel, &w.hitsB)
	w.upStandalone = newAccUpstream(t, accSoloModel, &w.hitsStandalone)

	w.bootstrap(t)
	return w
}

// bootstrap 通过 admin CRUD API 建立验收世界：
// 池 pool-alpha（node-a / node-b）+ 池外同规格目标 standalone + 三类客户端。
func (w *poolAcceptanceWorld) bootstrap(t *testing.T) {
	t.Helper()

	// 1. 建池。
	rec := w.admin(t, http.MethodPost, "/data/pools", map[string]any{
		"name":        accPoolName,
		"description": "acceptance pool",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 2. 池内 2 个成员 + 1 个同规格（openai、无池）目标。
	for _, node := range []struct{ name, endpoint, key string }{
		{"node-a", w.upA.URL, "key-a"},
		{"node-b", w.upB.URL, "key-b"},
	} {
		rec = w.admin(t, http.MethodPost, "/data/targets", map[string]any{
			"name":           node.name,
			"endpoint_type":  "openai",
			"usage_adapter":  "standard",
			"endpoint":       node.endpoint,
			"api_key":        node.key,
			"pool":           accPoolName,
			"model_mappings": []map[string]string{{"upstream": accPoolModel}},
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create target %s: expected 201, got %d (%s)", node.name, rec.Code, rec.Body.String())
		}
	}
	rec = w.admin(t, http.MethodPost, "/data/targets", map[string]any{
		"name":           "standalone",
		"endpoint_type":  "openai",
		"usage_adapter":  "standard",
		"endpoint":       w.upStandalone.URL,
		"api_key":        "key-solo",
		"model_mappings": []map[string]string{{"upstream": accSoloModel}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create standalone target: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 3. 三类客户端：仅池授权 / 旧式 allowed_targets / 无限制。
	for _, client := range []map[string]any{
		{"name": "pool-only", "access_key": "acc-pool-only", "allowed_targets": []string{}, "allowed_pools": []string{accPoolName}},
		{"name": "legacy", "access_key": "acc-legacy", "allowed_targets": []string{"standalone"}},
		{"name": "open", "access_key": "acc-open"},
	} {
		rec = w.admin(t, http.MethodPost, "/data/clients", client)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create client %v: expected 201, got %d (%s)", client["name"], rec.Code, rec.Body.String())
		}
	}
}

// admin 发送一次 admin JSON 请求并返回响应记录器。
func (w *poolAcceptanceWorld) admin(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal admin body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, "http://example.com"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	return rec
}

// proxyChat 以指定客户端身份发送一次 chat 请求（可显式指定目标），返回响应记录器。
func (w *poolAcceptanceWorld) proxyChat(t *testing.T, accessKey, model, explicitTarget, remoteIP string) *httptest.ResponseRecorder {
	t.Helper()
	principal, ok := w.authStore.Authenticate(accessKey)
	if !ok {
		t.Fatalf("authenticate client %q", accessKey)
	}
	body := bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	// 变 IP 避开连接粘连（affinity key = IP + 客户端 + 模型）。
	req.RemoteAddr = remoteIP + ":12345"
	if explicitTarget != "" {
		req.Header.Set("X-Proxy-Target", explicitTarget)
	}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rec := httptest.NewRecorder()
	w.service.ServeHTTP(rec, req)
	return rec
}

// TestPoolAcceptancePoolRoundRobinAndHeaders 验收主线 1：
// 2 节点池内请求轮询分布，且每个成功响应都带 X-Proxy-Target（节点名）
// 与 X-Proxy-Pool（池名）。
func TestPoolAcceptancePoolRoundRobinAndHeaders(t *testing.T) {
	w := setupPoolAcceptanceWorld(t)

	const total = 24
	counts := make(map[string]int)
	for i := 0; i < total; i++ {
		rr := w.proxyChat(t, "acc-pool-only", accPoolModel, "", fmt.Sprintf("10.3.%d.%d", i/250+1, i%250+1))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (%s)", i, rr.Code, rr.Body.String())
		}
		target := rr.Header().Get("X-Proxy-Target")
		if target != "node-a" && target != "node-b" {
			t.Fatalf("request %d: X-Proxy-Target must be a pool member, got %q", i, target)
		}
		if got := rr.Header().Get("X-Proxy-Pool"); got != accPoolName {
			t.Fatalf("request %d: expected X-Proxy-Pool=%q, got %q", i, accPoolName, got)
		}
		counts[target]++
	}
	if len(counts) != 2 {
		t.Fatalf("expected both pool members to serve, got distribution: %v", counts)
	}
	for name, n := range counts {
		if n < total/4 {
			t.Fatalf("member %s received %d of %d requests; round-robin must stay roughly uniform", name, n, total)
		}
	}
	// 服务端命中数与响应头口径互相印证。
	if w.hitsA.Load() == 0 || w.hitsB.Load() == 0 {
		t.Fatalf("expected both upstreams to receive traffic, got a=%d b=%d", w.hitsA.Load(), w.hitsB.Load())
	}
	if w.hitsStandalone.Load() != 0 {
		t.Fatalf("standalone must not serve %q (model not mapped there)", accPoolModel)
	}
}

// TestPoolAcceptanceClientAuthorizedByPoolOnly 验收主线 2 + 4：
// 仅授权池的客户端可访问成员、池外目标 403；同规格无池目标与旧式
// allowed_targets 客户端行为零回归（响应头不出现 X-Proxy-Pool）。
func TestPoolAcceptanceClientAuthorizedByPoolOnly(t *testing.T) {
	w := setupPoolAcceptanceWorld(t)

	// 仅池授权 → 成员可访问（显式 + 自动选择），带两个响应头。
	rr := w.proxyChat(t, "acc-pool-only", accPoolModel, "node-a", "10.4.0.1")
	if rr.Code != http.StatusOK {
		t.Fatalf("pool-only client → pool member: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "node-a" {
		t.Fatalf("expected X-Proxy-Target=node-a, got %q", got)
	}
	if got := rr.Header().Get("X-Proxy-Pool"); got != accPoolName {
		t.Fatalf("expected X-Proxy-Pool=%q, got %q", accPoolName, got)
	}
	rr = w.proxyChat(t, "acc-pool-only", accPoolModel, "", "10.4.0.2")
	if rr.Code != http.StatusOK {
		t.Fatalf("pool-only client automatic selection: expected 200, got %d", rr.Code)
	}

	// 仅池授权 → 池外目标 403（授权一池绝不等于授权全部）。
	rr = w.proxyChat(t, "acc-pool-only", accSoloModel, "standalone", "10.4.0.3")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("pool-only client → non-member target: expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}

	// 旧式客户端（allowed_targets、无池语义）零回归。
	rr = w.proxyChat(t, "acc-legacy", accSoloModel, "standalone", "10.4.0.4")
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy client → allowed target: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "standalone" {
		t.Fatalf("legacy client: expected X-Proxy-Target=standalone, got %q", got)
	}
	if got := rr.Header().Get("X-Proxy-Pool"); got != "" {
		t.Fatalf("pool-less target must NOT emit X-Proxy-Pool, got %q", got)
	}
	rr = w.proxyChat(t, "acc-legacy", accPoolModel, "node-a", "10.4.0.5")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("legacy client → unlisted target: expected 403, got %d", rr.Code)
	}

	// 无限制客户端语义不变（自动选择照常）。
	rr = w.proxyChat(t, "acc-open", accPoolModel, "", "10.4.0.6")
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted client: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestPoolAcceptancePoolPauseAndResume 验收主线 3：
// 池暂停使成员等效暂停（自动与显式选择均 503、状态页成员 paused），
// 池外目标不受影响；恢复后成员回归服务。
func TestPoolAcceptancePoolPauseAndResume(t *testing.T) {
	w := setupPoolAcceptanceWorld(t)

	// 暂停池（走 PUT /data/pools/{name}，body.name 必须与路径一致）。
	rec := w.admin(t, http.MethodPut, "/data/pools/"+accPoolName, map[string]any{
		"name":        accPoolName,
		"description": "acceptance pool",
		"paused":      true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("pause pool: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 列表接口反映 paused 状态。
	rec = w.admin(t, http.MethodGet, "/data/pools", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pools: expected 200, got %d", rec.Code)
	}
	var listResp struct {
		Pools []config.DeploymentPool `json:"pools"`
		Count int                     `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode pools list: %v", err)
	}
	if listResp.Count != 1 || len(listResp.Pools) != 1 || listResp.Pools[0].Name != accPoolName {
		t.Fatalf("unexpected pools list: %+v (count=%d)", listResp.Pools, listResp.Count)
	}
	if !listResp.Pools[0].Paused {
		t.Fatalf("pool %s must be paused after update", accPoolName)
	}

	// 成员等效暂停。显式选择成员 → 503（requested target paused）。
	// 自动选择：当池成员是该模型仅有的候选时，暂停成员被选择层过滤后
	// 落入既有 "model not supported by any target" 400 分支——这与把成员
	// 逐个单独暂停的行为完全一致（零回归不变量），因此断言 400 而非 503。
	rr := w.proxyChat(t, "acc-open", accPoolModel, "", "10.5.0.1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("automatic selection with paused pool: expected 400 (all model candidates paused), got %d (%s)", rr.Code, rr.Body.String())
	}
	rr = w.proxyChat(t, "acc-pool-only", accPoolModel, "node-a", "10.5.0.2")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("explicit paused member: expected 503, got %d (%s)", rr.Code, rr.Body.String())
	}

	// 状态口径：成员 Paused=true，池外目标不受波及。
	statuses := w.service.TargetStatuses(time.Now())
	byName := make(map[string]proxy.TargetStatus, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	for _, name := range []string{"node-a", "node-b", "standalone"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("target %s missing from TargetStatuses", name)
		}
	}
	for _, member := range []string{"node-a", "node-b"} {
		if !byName[member].Paused {
			t.Fatalf("pool paused: member %s must be effectively paused", member)
		}
	}
	if byName["standalone"].Paused {
		t.Fatal("non-member target must not be paused by pool pause")
	}

	// 池外目标在池暂停期间照常服务。
	rr = w.proxyChat(t, "acc-legacy", accSoloModel, "standalone", "10.5.0.3")
	if rr.Code != http.StatusOK {
		t.Fatalf("standalone during pool pause: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}

	// 恢复池 → 成员回归，响应头恢复。
	rec = w.admin(t, http.MethodPut, "/data/pools/"+accPoolName, map[string]any{
		"name":   accPoolName,
		"paused": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume pool: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	rr = w.proxyChat(t, "acc-pool-only", accPoolModel, "", "10.5.0.4")
	if rr.Code != http.StatusOK {
		t.Fatalf("after pool resume: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Proxy-Pool"); got != accPoolName {
		t.Fatalf("after pool resume: expected X-Proxy-Pool=%q, got %q", accPoolName, got)
	}
}

// TestPoolAcceptancePoolDeleteLifecycle 验收主线 5：
// 有成员时删除被拒（409 且错误含成员名）；摘除成员后删除成功，
// 且原成员作为无池目标继续服务（不再带 X-Proxy-Pool）。
func TestPoolAcceptancePoolDeleteLifecycle(t *testing.T) {
	w := setupPoolAcceptanceWorld(t)

	rec := w.admin(t, http.MethodDelete, "/data/pools/"+accPoolName, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete pool with members: expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "node-a") || !strings.Contains(body, "node-b") {
		t.Fatalf("409 error must name the blocking members, got: %s", body)
	}

	// 摘除成员（pool 置空）。
	for _, node := range []string{"node-a", "node-b"} {
		rec = w.admin(t, http.MethodPut, "/data/targets/"+node, map[string]any{"pool": ""})
		if rec.Code != http.StatusOK {
			t.Fatalf("unassign %s: expected 200, got %d (%s)", node, rec.Code, rec.Body.String())
		}
	}

	rec = w.admin(t, http.MethodDelete, "/data/pools/"+accPoolName, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete empty pool: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec = w.admin(t, http.MethodGet, "/data/pools", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pools after delete: expected 200, got %d", rec.Code)
	}
	var listResp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode pools list: %v", err)
	}
	if listResp.Count != 0 {
		t.Fatalf("expected 0 pools after delete, got %d", listResp.Count)
	}

	// 删除池后原成员以无池身份继续服务（用无限制客户端验证：
	// 池已不存在，仅池授权客户端按冻结语义不再获得任何目标）。
	rr := w.proxyChat(t, "acc-open", accPoolModel, "node-a", "10.6.0.1")
	if rr.Code != http.StatusOK {
		t.Fatalf("member after pool delete: expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "node-a" {
		t.Fatalf("expected X-Proxy-Target=node-a, got %q", got)
	}
	if got := rr.Header().Get("X-Proxy-Pool"); got != "" {
		t.Fatalf("pool deleted: member must no longer emit X-Proxy-Pool, got %q", got)
	}
}
