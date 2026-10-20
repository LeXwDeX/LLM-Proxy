package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// newPoolTestConfig 构造带池的最小可用配置：n 个同构成员（单 key、同模型）
// 隶属 poolName，可选附加一个池外目标。
func newPoolTestConfig(poolName string, memberURLs []string, extraTargets ...config.Target) *config.Config {
	targets := make([]config.Target, 0, len(memberURLs)+len(extraTargets))
	for i, u := range memberURLs {
		targets = append(targets, config.Target{
			Name:          fmt.Sprintf("member-%d", i+1),
			Endpoint:      u,
			APIKey:        fmt.Sprintf("key-%d", i+1),
			Pool:          poolName,
			ModelMappings: []config.ModelMapping{{Upstream: "pool-test-model"}},
		})
	}
	targets = append(targets, extraTargets...)
	return &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: targets,
		Pools:   []config.DeploymentPool{{Name: poolName}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}
}

// newPoolUpstream 返回一个返回固定 usage 的同构 mock 上游。
func newPoolUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"pool-test-model","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sendPoolRequest 通过 service 发送一次聊天请求，返回响应记录器。
func sendPoolRequest(t *testing.T, svc *Service, principal *auth.Principal, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	body := bytes.NewBufferString(`{"model":"pool-test-model","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.RemoteAddr = remoteAddr
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	return rr
}

func poolTestPrincipal(t *testing.T, name string) *auth.Principal {
	t.Helper()
	store := auth.NewStore()
	if err := store.LoadFromConfig([]config.Client{{Name: name, AccessKey: name + "-token"}}); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	p, ok := store.Authenticate(name + "-token")
	if !ok {
		t.Fatalf("authenticate %s", name)
	}
	return p
}

func TestApplyConfigPoolPauseMarksMembers(t *testing.T) {
	up1 := newPoolUpstream(t)
	up2 := newPoolUpstream(t)
	upOut := newPoolUpstream(t)

	cfg := newPoolTestConfig("pool-1", []string{up1.URL, up2.URL}, config.Target{
		Name:          "outsider",
		Endpoint:      upOut.URL,
		APIKey:        "key-out",
		ModelMappings: []config.ModelMapping{{Upstream: "pool-test-model"}},
	})
	cfg.Pools = []config.DeploymentPool{{Name: "pool-1", Paused: true}}

	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	statuses := svc.TargetStatuses(time.Now())
	byName := make(map[string]TargetStatus, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	for _, member := range []string{"member-1", "member-2"} {
		if !byName[member].Paused {
			t.Fatalf("pool paused: member %s must be effectively paused", member)
		}
	}
	if byName["outsider"].Paused {
		t.Fatal("non-member must not be paused by pool pause")
	}
}

func TestApplyConfigPoolPauseDoesNotMutateInput(t *testing.T) {
	up := newPoolUpstream(t)
	targets := []config.Target{{
		Name: "member-1", Endpoint: up.URL, APIKey: "key-1", Pool: "pool-1",
		ModelMappings: []config.ModelMapping{{Upstream: "pool-test-model"}},
	}}
	cfg := newPoolTestConfig("pool-1", nil)
	cfg.Pools = []config.DeploymentPool{{Name: "pool-1", Paused: true}}
	cfg.Targets = targets

	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_ = svc

	if targets[0].Paused {
		t.Fatal("applyPoolPause must not mutate the input slice")
	}
}

func TestPoolPauseResumePreservesKeyPool(t *testing.T) {
	up := newPoolUpstream(t)
	cfg := newPoolTestConfig("pool-1", nil)
	cfg.Targets = []config.Target{{
		Name: "member-1", Endpoint: up.URL,
		APIKey:        "key-primary",
		APIKeys:       []string{"key-a", "key-b"},
		Pool:          "pool-1",
		ModelMappings: []config.ModelMapping{{Upstream: "pool-test-model"}},
	}}

	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 手动屏蔽一个 key（模拟管理员屏蔽操作）。
	svc.mu.RLock()
	state := svc.targetsByName["member-1"]
	svc.mu.RUnlock()
	if state == nil || state.keyPool == nil {
		t.Fatalf("expected keyPool for member-1")
	}
	state.keyPool.blockKey(0)

	// 池暂停 → ApplyConfig；再恢复 → ApplyConfig。
	paused := cfg.Clone()
	paused.Pools[0].Paused = true
	if err := svc.ApplyConfig(paused); err != nil {
		t.Fatalf("ApplyConfig paused: %v", err)
	}
	resumed := cfg.Clone()
	if err := svc.ApplyConfig(resumed); err != nil {
		t.Fatalf("ApplyConfig resumed: %v", err)
	}

	svc.mu.RLock()
	stateAfter := svc.targetsByName["member-1"]
	svc.mu.RUnlock()
	if stateAfter == nil || stateAfter.keyPool == nil {
		t.Fatalf("expected keyPool after resume")
	}
	blocked := stateAfter.keyPool.blockedKeys()
	if len(blocked) != 1 {
		t.Fatalf("pause/resume cycle must preserve blocked keys, got: %v", blocked)
	}
	if _, ok := blocked["key-primary"]; !ok {
		t.Fatalf("expected key-primary to stay blocked, got: %v", blocked)
	}
}

func TestXProxyPoolHeaderPresentAndAbsent(t *testing.T) {
	// 有池目标：X-Proxy-Target 与 X-Proxy-Pool 同时出现。
	upPooled := newPoolUpstream(t)
	cfgPooled := newPoolTestConfig("pool-1", []string{upPooled.URL})
	svcPooled, err := NewService(cfgPooled, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rr := sendPoolRequest(t, svcPooled, poolTestPrincipal(t, "client-a"), "10.0.0.1:1000")
	if rr.Code != http.StatusOK {
		t.Fatalf("pooled target: expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "member-1" {
		t.Fatalf("expected X-Proxy-Target=member-1, got %q", got)
	}
	if got := rr.Header().Get("X-Proxy-Pool"); got != "pool-1" {
		t.Fatalf("expected X-Proxy-Pool=pool-1, got %q", got)
	}

	// 无池目标（红线回归）：X-Proxy-Target 保留，X-Proxy-Pool 必须不出现。
	upSolo := newPoolUpstream(t)
	cfgSolo := newPoolTestConfig("", nil)
	cfgSolo.Targets = []config.Target{{
		Name: "solo", Endpoint: upSolo.URL, APIKey: "key-solo",
		ModelMappings: []config.ModelMapping{{Upstream: "pool-test-model"}},
	}}
	cfgSolo.Pools = nil
	svcSolo, err := NewService(cfgSolo, newTestLogger())
	if err != nil {
		t.Fatalf("NewService solo: %v", err)
	}
	rr = sendPoolRequest(t, svcSolo, poolTestPrincipal(t, "client-b"), "10.0.0.2:1000")
	if rr.Code != http.StatusOK {
		t.Fatalf("solo target: expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "solo" {
		t.Fatalf("expected X-Proxy-Target=solo, got %q", got)
	}
	if got := rr.Header().Get("X-Proxy-Pool"); got != "" {
		t.Fatalf("pool-less target must NOT emit X-Proxy-Pool, got %q", got)
	}
}

func TestPoolMembersRoundRobinUniform(t *testing.T) {
	up1 := newPoolUpstream(t)
	up2 := newPoolUpstream(t)
	up3 := newPoolUpstream(t)

	cfg := newPoolTestConfig("pool-1", []string{up1.URL, up2.URL, up3.URL})
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const total = 300
	counts := make(map[string]int)
	principal := poolTestPrincipal(t, "rr-client")
	for i := 0; i < total; i++ {
		// 变 IP + 变客户端身份，避开粘连（affinity key = IP+client+model）。
		p := principal
		if i%2 == 1 {
			p = poolTestPrincipal(t, fmt.Sprintf("rr-client-%d", i%7))
		}
		rr := sendPoolRequest(t, svc, p, fmt.Sprintf("10.1.%d.%d:2000", i/250+1, i%250+1))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rr.Code)
		}
		counts[rr.Header().Get("X-Proxy-Target")]++
	}
	if len(counts) != 3 {
		t.Fatalf("expected all 3 pool members to serve, got: %v", counts)
	}
	for name, n := range counts {
		if n < 60 {
			t.Fatalf("member %s received %d requests, expected >= %d (uniform round-robin)", name, n, 60)
		}
	}
}

func TestPoolPausedAffinityDegradesToRoundRobin(t *testing.T) {
	// 红线 #3：粘连目标因池暂停不可用时必须降级轮询到池外可用目标，不能让请求失败。
	upPinned := newPoolUpstream(t)
	upFallback := newPoolUpstream(t)

	cfg := newPoolTestConfig("pool-1", []string{upPinned.URL}, config.Target{
		Name:          "fallback",
		Endpoint:      upFallback.URL,
		APIKey:        "key-fallback",
		ModelMappings: []config.ModelMapping{{Upstream: "pool-test-model"}},
	})
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// 同一客户端 + 同 IP 反复请求，构建粘连（无论粘连到哪个目标）。
	principal := poolTestPrincipal(t, "sticky-client")
	for i := 0; i < 5; i++ {
		rr := sendPoolRequest(t, svc, principal, "10.2.0.1:3000")
		if rr.Code != http.StatusOK {
			t.Fatalf("warmup %d: expected 200, got %d", i, rr.Code)
		}
	}

	// 池暂停后，同客户端继续请求：必须全部成功且绝不命中暂停成员。
	paused := cfg.Clone()
	paused.Pools[0].Paused = true
	if err := svc.ApplyConfig(paused); err != nil {
		t.Fatalf("ApplyConfig paused: %v", err)
	}
	for i := 0; i < 10; i++ {
		rr := sendPoolRequest(t, svc, principal, "10.2.0.1:3000")
		if rr.Code != http.StatusOK {
			t.Fatalf("after pool pause %d: expected 200 (affinity degrade), got %d", i, rr.Code)
		}
		if got := rr.Header().Get("X-Proxy-Target"); got == "member-1" {
			t.Fatalf("request %d hit pool-paused member despite pause", i)
		}
	}
}
