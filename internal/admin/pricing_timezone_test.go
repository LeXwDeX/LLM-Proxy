package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
)

// UTC 口径：模板时区一经保存，更新请求省略 timezone 时必须保持原值，
// 不得被默认的 +08:00 无声覆盖（否则历史价格的时段边界会漂移 8 小时）。
func TestUpdatePricingTemplateKeepsStoredTimezoneWhenOmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := testConfig(dir, 1, []string{"k1"})
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)
	if err := stores.targetStore.Create(config.Target{Name: "A", AllowBearer: true, UsageAdapter: "standard", Endpoint: "https://example.com", ModelMappings: []config.ModelMapping{{Upstream: "fable5"}}}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "pricing-tz-test", CookieName: "session", TTLSeconds: 3600}, logger)
	svc, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(config.NewManager(path), auth.NewStore(), svc, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)
	_, cookie, _ := sm.CreateSession("admin", RoleAdmin)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	decode := func(rr *httptest.ResponseRecorder) pricing.PricingTemplate {
		var out struct {
			Template pricing.PricingTemplate `json:"template"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Template
	}
	periods := `"periods":[{"weekdays":[1,2,3,4,5,6,7],"start":"09:00","end":"17:00","rates":{"input_per_1m_tokens":4}}]`
	// 模板 0（全天兜底）必须是 flat；时段模板需要显式生效起点。
	if rr := do("POST", "/data/pricing-templates", `{"target":"A","model":"fable5","base_rates":{"input_per_1m_tokens":1}}`); rr.Code != 201 {
		t.Fatalf("template 0: %d %s", rr.Code, rr.Body)
	}
	rr := do("POST", "/data/pricing-templates", `{"target":"A","model":"fable5","mode":"schedule","start":"2026-01-01T00:00:00Z","end":"2027-01-01T00:00:00Z","timezone":"+00:00","base_rates":{"input_per_1m_tokens":2},`+periods+`}`)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	created := decode(rr)
	if created.Timezone != "+00:00" {
		t.Fatalf("created timezone=%q", created.Timezone)
	}
	// 只改价格，请求体省略 timezone。
	rr = do("PUT", "/data/pricing-templates/"+created.ID, `{"target":"A","model":"fable5","mode":"schedule","start":"2026-01-01T00:00:00Z","end":"2027-01-01T00:00:00Z","base_rates":{"input_per_1m_tokens":3},`+periods+`}`)
	if rr.Code != 200 {
		t.Fatalf("update: %d %s", rr.Code, rr.Body)
	}
	if got := decode(rr).Timezone; got != "+00:00" {
		t.Fatalf("update without timezone changed stored timezone to %q, want +00:00", got)
	}
	// 显式给出时区仍然生效。
	rr = do("PUT", "/data/pricing-templates/"+created.ID, `{"target":"A","model":"fable5","mode":"schedule","start":"2026-01-01T00:00:00Z","end":"2027-01-01T00:00:00Z","timezone":"+08:00","base_rates":{"input_per_1m_tokens":3},`+periods+`}`)
	if rr.Code != 200 || decode(rr).Timezone != "+08:00" {
		t.Fatalf("explicit timezone must apply: %d %s", rr.Code, rr.Body)
	}
	// 新建时省略 timezone 仍落盘为默认 +08:00（冻结既有默认语义）。
	rr = do("POST", "/data/pricing-templates", `{"target":"A","model":"fable5","mode":"schedule","start":"2030-01-01T00:00:00Z","base_rates":{"input_per_1m_tokens":2},`+periods+`}`)
	if rr.Code != 201 || decode(rr).Timezone != "+08:00" {
		t.Fatalf("create without timezone: %d %s", rr.Code, rr.Body)
	}
}
