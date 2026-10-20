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

func TestSharedPricingAdminBindingsAndLegacyProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := testConfig(dir, 1, []string{"k1"})
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)
	for _, name := range []string{"A", "B"} {
		if err := stores.targetStore.Create(config.Target{Name: name, AllowBearer: true, UsageAdapter: "standard", Endpoint: "https://example.com", ModelMappings: []config.ModelMapping{{Upstream: "fable5"}, {Upstream: "fable5-1"}}}); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "shared-pricing-test", CookieName: "session", TTLSeconds: 3600}, logger)
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
	body := `{"targets":["A","B"],"models":["fable5","fable5-1"],"base_rates":{"input_per_1m_tokens":3}}`
	rr := do("POST", "/data/pricing-templates", body)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	var created struct {
		Template pricing.PricingTemplate `json:"template"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Template.ID
	if len(created.Template.Targets) != 2 || len(created.Template.Models) != 2 || created.Template.Target != "A" || created.Template.Model != "fable5" {
		t.Fatalf("response binding contract: %+v", created.Template)
	}
	rr = do("PUT", "/data/pricing-templates/"+id, `{"targets":["b","a"],"models":["FABLE5-1","FABLE5"],"base_rates":{"input_per_1m_tokens":3}}`)
	if rr.Code != 200 {
		t.Fatalf("set-equivalent update rejected: %d %s", rr.Code, rr.Body)
	}
	rr = do("GET", "/data/pricing-templates?target=B&model=fable5-1", "")
	if !strings.Contains(rr.Body.String(), id) {
		t.Fatalf("second binding missing: %s", rr.Body)
	}
	rr = do("PUT", "/data/pricing-templates/"+id, strings.Replace(body, ":3", ":9", 1))
	if rr.Code != 200 {
		t.Fatalf("update: %d %s", rr.Code, rr.Body)
	}
	rr = do("PUT", "/data/pricing-templates/"+id, `{"target":"A","model":"fable5"}`)
	if rr.Code != 400 {
		t.Fatalf("legacy update shared rule: %d %s", rr.Code, rr.Body)
	}
	rr = do("POST", "/data/pricing-templates", `{"target":"B","model":"fable5-1"}`)
	if rr.Code != 400 {
		t.Fatalf("legacy create overwrote shared rule: %d %s", rr.Code, rr.Body)
	}
	rr = do("POST", "/data/pricing-templates", `{"targets":["A","missing"],"models":["fable5"]}`)
	if rr.Code != 400 {
		t.Fatalf("missing target accepted: %d %s", rr.Code, rr.Body)
	}
	rr = do("POST", "/data/pricing-templates", `{"targets":["A"],"models":["no-such-model"]}`)
	if rr.Code != 400 {
		t.Fatalf("unconfigured model accepted: %d %s", rr.Code, rr.Body)
	}
	rr = do("POST", "/data/pricing-templates", `{"targets":["","A"],"models":["fable5"]}`)
	if rr.Code != 400 {
		t.Fatalf("mixed global accepted: %d %s", rr.Code, rr.Body)
	}
	// C intentionally lacks the second model: one invalid Cartesian-product
	// binding rejects the whole new rule before any data is stored.
	if err := stores.targetStore.Create(config.Target{Name: "C", AllowBearer: true, UsageAdapter: "standard", Endpoint: "https://example.com", ModelMappings: []config.ModelMapping{{Upstream: "fable5"}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.pricingTemplateStore.List()
	rr = do("POST", "/data/pricing-templates", `{"targets":["C","A"],"models":["fable5","fable5-1"],"start":"2027-01-01T00:00:00Z"}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "fable5-1") {
		t.Fatalf("partial mapping accepted: %d %s", rr.Code, rr.Body)
	}
	after, _ := h.pricingTemplateStore.List()
	if len(before) != len(after) {
		t.Fatal("failed binding validation wrote a rule")
	}
	_, readonlyCookie, _ := sm.CreateSession("viewer", RoleReadonly)
	req := httptest.NewRequest("POST", "/data/pricing-templates", strings.NewReader(body))
	req.AddCookie(readonlyCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("readonly shared write: %d", rr.Code)
	}
	global := `{"targets":[""],"models":["global-a","global-b"]}`
	rr = do("POST", "/data/pricing-templates", global)
	if rr.Code != 201 {
		t.Fatalf("global: %d %s", rr.Code, rr.Body)
	}
	rr = do("PUT", "/data/model-costs/global-b", `{"input_per_1m_tokens":8}`)
	if rr.Code != 400 {
		t.Fatalf("legacy cost update shared: %d %s", rr.Code, rr.Body)
	}
	rr = do("DELETE", "/data/model-costs/global-a", "")
	if rr.Code != 400 {
		t.Fatalf("legacy cost delete shared: %d %s", rr.Code, rr.Body)
	}
	rr = do("GET", "/data/model-costs", "")
	if !strings.Contains(rr.Body.String(), `"global-a"`) || !strings.Contains(rr.Body.String(), `"global-b"`) {
		t.Fatalf("legacy projection missing models: %s", rr.Body)
	}
}
