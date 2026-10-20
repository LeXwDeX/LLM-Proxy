// quota_serve_test.go — 测试 ServeHTTP 的 429 配额准入检查。
//
// 测试策略：使用真实的 quota.Manager（与 nosql stores 搭配），
// 通过 nosql.BackfillUsageAgg 聚合 usage events 并调用 Manager.Evaluate 触发超限标记，
// 再注入到 proxy.Service 中验证 ServeHTTP 返回 429 + OpenAI 兼容 JSON。
package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// buildQuotaExceededManager 构造一个真实的 quota.Manager，其中 clientName 已触发达日超限。
// 使用 bbolt 持久化 stores，保证与生产代码完全一致的 Evaluate 行为。
// 返回 manager 与 cleanup 闭包（调用者 defer 之）。
func buildQuotaExceededManager(t *testing.T, clientName string) *quota.Manager {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "quota-test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	clientStore := nosql.NewClientStore(db)
	usageStore := nosql.NewUsageStore(db)
	modelCostStore := nosql.NewModelCostStore(db)

	// 创建 client，设置每日 quota = $1
	if err := clientStore.Create(config.Client{
		Name:           clientName,
		AccessKey:      "test-key",
		IPAddress:      "192.0.2.1",
		QuotaDailyUSD:  1.0,
		AllowedTargets: []string{},
	}); err != nil {
		t.Fatalf("Create client failed: %v", err)
	}

	// 创建模型费用：$30/1M input tokens（100K input = $3，远超 $1 日配额）
	if err := modelCostStore.Upsert(nosql.ModelCost{
		Model:                "gpt-4",
		InputPer1MTokens:     30.0,
		OutputPer1MTokens:    60.0,
		CacheWritePer1MToken: 0.0,
	}); err != nil {
		t.Fatalf("Upsert model cost failed: %v", err)
	}

	// 写入一条超出配额的 usage event（当前小时内 200K input tokens）
	now := time.Now().UTC()
	if err := usageStore.Record(usage.Event{
		Timestamp:    now,
		ClientName:   clientName,
		EndpointType: "openai",
		Model:        "gpt-4",
		InputTokens:  200_000, // 200K * $30/1M = $6 > $1
		OutputTokens: 0,
		StatusCode:   200,
	}); err != nil {
		t.Fatalf("Record usage failed: %v", err)
	}

	// 聚合到 hourly bucket
	if err := nosql.BackfillUsageAgg(db); err != nil {
		t.Fatalf("BackfillUsageAgg failed: %v", err)
	}

	// 构造 manager 并触发 Evaluate
	cat, _ := catalog.New()
	mgr, err := quota.New(quota.Options{
		Catalog:     cat,
		CostStore:   modelCostStore,
		UsageStore:  usageStore,
		ClientStore: clientStore,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("quota.New failed: %v", err)
	}
	mgr.Evaluate(clientName)

	return mgr
}

// makeRequestForQuotaCheck 构造一个带有 principal 的请求并调用 ServeHTTP。
// 返回 recorder 供断言。
func makeRequestForQuotaCheck(t *testing.T, svc *Service, authStore *auth.Store, accessKey string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))

	// 从 auth store 获取 principal（与 auth middleware 写入的相同）
	principal, ok := authStore.Authenticate(accessKey)
	if !ok {
		t.Fatalf("Authenticate(%q) failed", accessKey)
	}
	ctx := auth.WithPrincipal(r.Context(), principal)
	r = r.WithContext(ctx)

	svc.ServeHTTP(rec, r)
	return rec
}

// TestServeHTTP_QuotaExceeded_429 验证 client 配额超限时返回 429 + OpenAI 兼容 JSON 错误体。
func TestServeHTTP_QuotaExceeded_429(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 至少需要一个 mock 上游 target（即使 429 不会到达转发阶段，
	// Service 构造需要 cfg.Targets 不为空，避免 NewService 报错）
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4"}`))
	}))
	defer mockUpstream.Close()
	mockURL, _ := url.Parse(mockUpstream.URL)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "mock-azure",
				EndpointType:       "azure_openai",
				Endpoint:           mockURL.String(),
				APIKey:             "k1",
				ResourcePathPrefix: "/openai/deployments/gpt-4/chat/completions",
			},
		},
	}

	svc, err := NewService(cfg, logger)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	// 构造已超限的 manager
	mgr := buildQuotaExceededManager(t, "alice")
	svc.SetQuotaManager(mgr)

	// 验证：检查状态
	info, exceeded := mgr.Check("alice")
	if !exceeded {
		t.Fatalf("setup: expected client to be exceeded after Evaluate, got info=%+v", info)
	}

	// 构造 auth store（与 quota manager 使用相同的 client 列表）
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig([]config.Client{
		{Name: "alice", AccessKey: "test-key", AllowedTargets: []string{}},
	}); err != nil {
		t.Fatalf("LoadFromConfig failed: %v", err)
	}

	rec := makeRequestForQuotaCheck(t, svc, authStore, "test-key")

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d. body=%s", rec.Code, rec.Body.String())
	}

	// 校验 Content-Type
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// 校验 OpenAI 兼容 JSON 字段齐全
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Quota   struct {
				Scope     string  `json:"scope"`
				GroupName string  `json:"group_name"`
				Dimension string  `json:"dimension"`
				LimitUSD  float64 `json:"limit_usd"`
				UsedUSD   float64 `json:"used_usd"`
				ResetsAt  string  `json:"resets_at"`
			} `json:"quota"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response failed: %v. body=%s", err, rec.Body.String())
	}

	if body.Error.Message == "" {
		t.Error("expected error.message populated")
	}
	if body.Error.Type != "quota_exceeded" {
		t.Errorf("expected error.type 'quota_exceeded', got %q", body.Error.Type)
	}
	if body.Error.Code != "quota_exceeded" {
		t.Errorf("expected error.code 'quota_exceeded', got %q", body.Error.Code)
	}
	// 额度体系 v2：客户端级拒绝只剩「未绑职级」（scope=job_level），
	// 不带 limit/used（未配置额度，而非用量超限）。
	if body.Error.Quota.Scope != "job_level" {
		t.Errorf("expected error.quota.scope 'job_level', got %q", body.Error.Quota.Scope)
	}
	if body.Error.Quota.GroupName != "" {
		t.Errorf("expected job_level error.quota.group_name empty, got %q", body.Error.Quota.GroupName)
	}
	if body.Error.Quota.Dimension != "unassigned" {
		t.Errorf("expected error.quota.dimension 'unassigned', got %q", body.Error.Quota.Dimension)
	}
	if body.Error.Quota.LimitUSD != 0 || body.Error.Quota.UsedUSD != 0 {
		t.Errorf("expected zero limit/used for job_level scope, got %v/%v", body.Error.Quota.LimitUSD, body.Error.Quota.UsedUSD)
	}
	if !strings.Contains(body.Error.Message, "job level") {
		t.Errorf("expected job-level specific message, got %q", body.Error.Message)
	}
}

// TestServeHTTP_QuotaManagerNil_PassThrough 验证 quotaManager 为 nil 时不拦截请求。
func TestServeHTTP_QuotaManagerNil_PassThrough(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 不设置 quotaManager（默认 nil）
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4","usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	defer mockUpstream.Close()
	mockURL, _ := url.Parse(mockUpstream.URL)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "mock-azure",
				EndpointType:       "azure_openai",
				Endpoint:           mockURL.String(),
				APIKey:             "k1",
				ResourcePathPrefix: "/openai/deployments/gpt-4/chat/completions",
			},
		},
	}

	svc, err := NewService(cfg, logger)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	// 不 set quotaManager（保持 nil）

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig([]config.Client{
		{Name: "alice", AccessKey: "test-key", AllowedTargets: []string{}},
	}); err != nil {
		t.Fatalf("LoadFromConfig failed: %v", err)
	}

	rec := makeRequestForQuotaCheck(t, svc, authStore, "test-key")

	// 不应返回 429（请求应该到达 mock upstream 或返回其他业务错误）
	if rec.Code == http.StatusTooManyRequests {
		t.Errorf("quota manager is nil, should not return 429. body=%s", rec.Body.String())
	}
}

// TestServeHTTP_QuotaNotExceeded_PassThrough 验证 client 未超限时不拦截请求。
func TestServeHTTP_QuotaNotExceeded_PassThrough(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 创建 manager，但对一个无配额设置的 client 调用 Check，应该返回 false
	cat, _ := catalog.New()
	mgr, err := quota.New(quota.Options{
		Catalog: cat,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("quota.New failed: %v", err)
	}

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4","usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	defer mockUpstream.Close()
	mockURL, _ := url.Parse(mockUpstream.URL)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "mock-azure",
				EndpointType:       "azure_openai",
				Endpoint:           mockURL.String(),
				APIKey:             "k1",
				ResourcePathPrefix: "/openai/deployments/gpt-4/chat/completions",
			},
		},
	}

	svc, err := NewService(cfg, logger)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	svc.SetQuotaManager(mgr)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig([]config.Client{
		{Name: "alice", AccessKey: "test-key", AllowedTargets: []string{}},
	}); err != nil {
		t.Fatalf("LoadFromConfig failed: %v", err)
	}

	rec := makeRequestForQuotaCheck(t, svc, authStore, "test-key")
	if rec.Code == http.StatusTooManyRequests {
		t.Errorf("client not exceeded, should not return 429. body=%s", rec.Body.String())
	}
}
