// lb_audit_test.go — 负载功能模块边缘效应审计的回归测试。
// 覆盖 HIGH-1（多 key 目标 5xx 无界轮换）、MED-2（muted 排除权重）、
// MED-3（收敛透传按失败计）、LOW-67（all-exhausted 可 failover）、
// LOW-8910（429 退避感知 ctx 取消）。
package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// TestService503MultiKeyNoRetryStorm 守护 HIGH-1：多 key 目标对"非 429、非 key 耗尽"
// 的上游错误（503）不得在 key 池里无界轮换。修复后应最多每 key 一次（实际为 1 次），
// 且 ServeHTTP 快速返回；修复前会一直打到 5s 请求超时（hang）。
func TestService503MultiKeyNoRetryStorm(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy"}`)) // 非 key 耗尽响应体
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "k1",              // 多 key 目标 → keyPool != nil
			APIKeys:            []string{"k2", "k3"},
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	done := make(chan struct{})
	go func() {
		body := bytes.NewBufferString(`{"model":"gpt-4o"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatalf("ServeHTTP did not return within 4s; unbounded key rotation retry storm (attempts=%d)", attempts.Load())
	}

	// 修复后：503 非 key 问题，换 key 无意义，最多每 key 一次（实际 1 次）。
	if got := attempts.Load(); got > 3 {
		t.Fatalf("expected at most 3 upstream calls (one per key) for 503 on multi-key target, got %d (retry storm)", got)
	}
}

// TestFindAvailableTargetMutedExcludedFromWeight 守护 MED-2：muted 目标不得参与加权分配。
// 当存在健康目标时，findAvailableTarget 应总是返回健康目标且 muted==nil；
// 修复前 muted 目标仍在候选集 + 计入权重，加权命中时会以 mutedCandidate 形式返回（非 nil）。
func TestFindAvailableTargetMutedExcludedFromWeight(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstreamB.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "target-a", Endpoint: upstreamA.URL, ResourcePathPrefix: "/openai", APIKey: "ka"},
			{Name: "target-b", Endpoint: upstreamB.URL, ResourcePathPrefix: "/openai", APIKey: "kb"},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	aState, ok := service.targetByName("target-a")
	if !ok {
		t.Fatal("target-a not found")
	}
	aState.MarkFailure(time.Now(), 60*time.Second) // mute target-a

	for i := 0; i < 100; i++ {
		state, muted := service.findAvailableTarget(nil, nil, time.Now())
		if state == nil {
			t.Fatalf("iter %d: expected healthy target-b, got nil", i)
		}
		if state.Target().Name == "target-a" {
			t.Fatalf("iter %d: muted target-a was selected", i)
		}
		if muted != nil {
			t.Fatalf("iter %d: expected nil muted (healthy target exists), got %s", i, muted.Target().Name)
		}
	}
}

// TestConvergenceAccountFailureMarksTargetFailure 守护 MED-3：收敛透传的账户级 4xx（401）
// 应按目标失败计（mute），而非成功。两个 key 都返回 invalid_token → 收敛 → 透传 401 给客户端，
// 但目标应被 mute。修复前 writeResponse 把 401 当成功（isUpstreamFailureStatus 不含 401）→ 不 mute。
func TestConvergenceAccountFailureMarksTargetFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// 触发 isKeyExhausted 401 分支 → invalid_token（账户级硬失败）
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "k1",
			APIKeys:            []string{"k2"},
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// 透传真实 401 给客户端（收敛的设计目的）。
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 passthrough, got %d body=%s", rr.Code, rr.Body.String())
	}
	// 关键断言：账户级失败应 mute 目标（修复前是 MarkSuccess → 不 mute）。
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 {
		t.Fatalf("expected 1 target status, got %d", len(statuses))
	}
	if !statuses[0].Muted {
		t.Fatalf("expected target to be muted after account-level convergence failure, but it was marked healthy (MED-3)")
	}
	metrics := service.MetricsSnapshot()
	if metrics.TotalSuccess != 0 {
		t.Fatalf("expected 0 success (account-level failure), got %d", metrics.TotalSuccess)
	}
}

// TestForwardRequestAllKeysExhaustedIsRetryable 守护 LOW-67(1)：当某 target 所有 key 耗尽，
// forwardRequest 返回的错误应是 retryable，使 service.go 能 failover 到其他目标。
// 修复前 retryable=false → 客户端直接吃 503，即便其他目标健康。
func TestForwardRequestAllKeysExhaustedIsRetryable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "k1",
			APIKeys:            []string{"k2"},
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	state, ok := service.targetByName("openai")
	if !ok || state.keyPool == nil {
		t.Fatalf("expected openai with key pool, ok=%v", ok)
	}
	// 预先耗尽所有 key（账户级硬失败，冷却 5min，唤醒探测不会恢复）。
	for i := range state.keyPool.entries {
		state.keyPool.markExhaustedWithError(i, "invalid_token", "hard fail")
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	_, _, _, fErr := service.forwardRequest(req, state, []byte(`{"model":"gpt-4o"}`), "", nil)
	if fErr == nil {
		t.Fatal("expected error when all keys exhausted")
	}
	fe, ok := fErr.(*forwardAttemptError)
	if !ok {
		t.Fatalf("expected *forwardAttemptError, got %T", fErr)
	}
	if !fe.retryable {
		t.Fatalf("expected retryable=true so service.go can failover to other targets (LOW-67), got retryable=false (status=%d)", fe.status)
	}
}

// TestService429BackoffContextAware 守护 LOW-8910(1)：多 key 目标整体 429 限流时，
// 退避 Sleep 必须感知客户端取消。修复前 time.Sleep 忽略 ctx，单个请求会阻塞 ~keys*3s。
func TestService429BackoffContextAware(t *testing.T) {
	const numKeys = 4
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer upstream.Close()

	keys := make([]string, 0, numKeys)
	for i := 0; i < numKeys; i++ {
		keys = append(keys, "k"+string(rune('1'+i)))
	}
	cfg := &config.Config{
		Server:   config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 60},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             keys[0],
			APIKeys:            keys[1:],
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(ctx, principal))

	done := make(chan struct{})
	start := time.Now()
	go func() {
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond) // 等请求进入第一次 429 退避
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("ServeHTTP did not return within 2s of context cancel — 429 backoff Sleep is not context-aware (LOW-8910)")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("ServeHTTP took %v after cancel; backoff ignored context cancellation (expected <1.5s)", elapsed)
	}
}

// TestExplicitTargetNotMutedOnAllKeysExhausted 守护评审回归修复：显式目标（X-Proxy-Target）
// 所有 key 耗尽时，forwardRequest 返回 retryable+skipMute 错误。service.go failover 分支因
// skipMute 跳过 handleForwardError，不 mute 目标——否则后续显式请求会被 selectTarget 的 IsMuted
// 短路 60s，即便 key 已恢复也无法服务。
func TestExplicitTargetNotMutedOnAllKeysExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "k1",
			APIKeys:            []string{"k2"},
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	state, ok := service.targetByName("openai")
	if !ok || state.keyPool == nil {
		t.Fatalf("expected openai with key pool, ok=%v", ok)
	}
	for i := range state.keyPool.entries {
		state.keyPool.markExhaustedWithError(i, "invalid_token", "hard fail")
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req.Header.Set("X-Proxy-Target", "openai") // 显式目标 → allowFallback=false
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// 显式目标所有 key 耗尽：WP-1 统一为 429 返回给客户端（原 503）。
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rr.Code, rr.Body.String())
	}
	// 关键：目标不应被 mute（skipMute）。修复前 handleForwardError 会 MarkFailure → mute 60s。
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 {
		t.Fatalf("expected 1 target status, got %d", len(statuses))
	}
	if statuses[0].Muted {
		t.Fatalf("explicit target must NOT be muted on all-keys-exhausted (skipMute), but NextRetry=%s", statuses[0].MutedUntil)
	}
}

// TestServiceGLMRateLimitFailover 守护 GLM 限流识别+failover：
// 智谱限流是 HTTP 200 + SSE event:error + rate_limit_error（code 1308/1310/1302），
// 非 HTTP 429。修复前 detectGLMRateLimitInBody 只认 1308，1310/1302 当正常流透传 →
// key 不静默、不 failover。全 key 进入限流后请求卡死在 glm target。
// 修复后：1310/1302 也被识别 → markExhausted → 全 key 耗尽上浮 retryable → failover 到 fallback。
func TestServiceGLMRateLimitFailover(t *testing.T) {
	var glmHits, fallbackHits atomic.Int64
	glmUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		glmHits.Add(1)
		// 模拟智普周/月限流：HTTP 200 + SSE rate_limit_error code 1310 + 重置时间
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"1310\",\"message\":\"[1310][您已达到每周/每月使用上限，您的限额将在 2099-01-01 00:00:00 重置。][x]\"}}\n\ndata: [DONE]\n"))
	}))
	defer glmUpstream.Close()
	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer fallbackUpstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "glm-1", EndpointType: config.EndpointTypeClaude, Endpoint: glmUpstream.URL, UsageAdapter: config.UsageAdapterGLM, APIKeys: []string{"k1", "k2"}},
			{Name: "fallback", EndpointType: config.EndpointTypeClaude, Endpoint: fallbackUpstream.URL, APIKey: "fk1"},
		},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"glm-4.6"}`))
	req.RemoteAddr = "1.2.3.4:1234"
	// 预置粘连到 glm-1，强制首轮选 glm target（触发全 key 限流后再 failover 到 fallback）。
	service.affinity.Set(affinityKey(clientIP(req), "tester", "glm-4.6"), "glm-1", time.Now())
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	done := make(chan struct{})
	var rr *httptest.ResponseRecorder
	go func() {
		rr = httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatalf("ServeHTTP did not return within 6s; GLM all-key rate limit stuck on target (glmHits=%d)", glmHits.Load())
	}

	// ① 1310 = 额度类，RETRY<2 不再做"坚持同 key + sleep 1s"，直接换 key + reqTotalFails++。
	//    每个 key 只尝试一次（共 2 次），不再是旧逻辑的每 key 2 次（共 4 次）。
	//    fix-glm-ratelimit G3：额度类强制换 key，避免 1 秒后必然再撞限。
	if got := glmHits.Load(); got != 2 {
		t.Fatalf("expected 2 glm attempts (1 per key, no same-key retry for quota class), got %d", got)
	}
	// ② failover 生效：fallback target 收到请求。
	if fallbackHits.Load() != 1 {
		t.Fatalf("expected failover to fallback target (1 hit), got %d", fallbackHits.Load())
	}
	// ③ 最终成功（修复前会透传 200 限流 SSE 给客户端，但走的是 MarkSuccess 假成功路径，
	//    且无 fallback hit）。这里要求真正 failover 成功。
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d body=%s", rr.Code, rr.Body.String())
	}
	// ④ glm 两个 key 都被静默，reason 为周/月额度限流。
	glmState, _ := service.targetByName("glm-1")
	for i, e := range glmState.keyPool.entries {
		if !e.exhausted {
			t.Errorf("glm-1 key[%d] should be marked exhausted (rate limited), but it's active", i)
		}
		if e.exhaustReason != glmRateLimitQuota {
			t.Errorf("glm-1 key[%d] reason = %q, want %q", i, e.exhaustReason, glmRateLimitQuota)
		}
	}
	// ⑤ glm target 不应被 mute（skipMute=true，keypool 冷却自管）。
	if glmState.IsMuted(time.Now()) {
		t.Fatalf("glm target must NOT be muted on all-key rate limit (skipMute), but NextRetry=%s", glmState.NextRetry())
	}
}

// TestServiceGLM1305_FailoverAndAutoRecover 守护 fix-glm-ratelimit G1 + G3：
// 智谱 1305 (服务过载) 通过 429 路径投递时，所有 key 过载后应 failover 到 fallback target；
// 60s 冷却到期后 key 自动恢复（isRecoverableAfterCooldown = true）。
// 与 TestServiceGLMRateLimitFailover 的差异：
//   - 投递路径不同：429 JSON (parse429Body) vs 200+SSE (detectGLMRateLimitInBody)
//   - 限流类型不同：1305 (瞬时 60s) vs 1310 (额度类 5h/周)
//   - 恢复时间不同：60s vs 5h+
func TestServiceGLM1305_FailoverAndAutoRecover(t *testing.T) {
	var glmHits, fallbackHits atomic.Int64
	glmUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		glmHits.Add(1)
		// 1305：平台服务过载，429 + JSON body（智谱实际投递路径）
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"1305","message":"服务暂不可用"}}`))
	}))
	defer glmUpstream.Close()
	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer fallbackUpstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{
			{Name: "glm-overload", EndpointType: config.EndpointTypeClaude, Endpoint: glmUpstream.URL, UsageAdapter: config.UsageAdapterGLM, APIKeys: []string{"k1", "k2"}},
			{Name: "fallback", EndpointType: config.EndpointTypeClaude, Endpoint: fallbackUpstream.URL, APIKey: "fk1"},
		},
		Logging: config.LoggingConfig{Level: "error", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"glm-5"}`))
	req.RemoteAddr = "1.2.3.4:1234"
	service.affinity.Set(affinityKey(clientIP(req), "tester", "glm-5"), "glm-overload", time.Now())
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	// ① 1305 = 瞬时类，RETRY<2 做同 key 重试：每个 key 2 次（共 4 次尝试）。
	if got := glmHits.Load(); got != 4 {
		t.Fatalf("expected 4 glm attempts (2 per key for transient class with RETRY<2), got %d", got)
	}
	// ② failover 到 fallback
	if fallbackHits.Load() != 1 {
		t.Fatalf("expected 1 fallback hit, got %d", fallbackHits.Load())
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 via failover, got %d body=%s", rr.Code, rr.Body.String())
	}

	// ③ 两个 glm key 都被静默，reason 为服务过载
	glmState, _ := service.targetByName("glm-overload")
	for i, e := range glmState.keyPool.entries {
		if !e.exhausted {
			t.Errorf("key[%d] should be exhausted", i)
		}
		if e.exhaustReason != glmRateLimitOverload {
			t.Errorf("key[%d] reason = %q, want %q (1305)", i, e.exhaustReason, glmRateLimitOverload)
		}
	}

	// ④ 60s 冷却到期后 key 自动恢复
	for i := range glmState.keyPool.entries {
		// 模拟冷却过期（直接修改冷却结束时间到过去）
		glmState.keyPool.entries[i].cooldownEnd = time.Now().Add(-1 * time.Second)
	}
	// selectKey 应能从已冷却过期的 key 中选一个（isRecoverableAfterCooldown = true）
	key, idx := glmState.keyPool.selectKey()
	if key == "" || idx == -1 {
		t.Errorf("60s-cooldown-expired key should auto-recover, got (key=%q, idx=%d)", key, idx)
	}

	// ⑤ GLM 限流不应 mute 目标
	if glmState.IsMuted(time.Now()) {
		t.Fatalf("glm target must NOT be muted on 1305 overload (skipMute via 429 path)")
	}
}
