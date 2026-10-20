package admin

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagePricesAdminRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := testConfig(dir, 1, []string{"k1"})
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := NewSessionManager(config.AdminSessionConfig{Secret: "test-image-secret", CookieName: "image-session", TTLSeconds: 3600}, logger)
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(config.NewManager(path), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)
	_, cookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	body := `{"target":"","model":"image-model","mode":"flat","base_rates":{"image_input_per_1m_tokens":1.234567,"cached_image_input_per_1m_tokens":0,"image_output_per_1m_tokens":9.876543}}`
	rr := do("POST", "/data/pricing-templates", body)
	if rr.Code != 201 {
		t.Fatalf("create %d %s", rr.Code, rr.Body)
	}
	var created struct {
		Template pricing.PricingTemplate `json:"template"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	check := func(want float64) {
		t.Helper()
		rr := do("GET", "/data/pricing-templates?model=image-model", "")
		var got struct {
			Templates []pricing.PricingTemplate `json:"templates"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Templates) != 1 {
			t.Fatalf("response %s", rr.Body)
		}
		r := got.Templates[0].BaseRates
		if r.ImageInputPer1MTokens == nil || *r.ImageInputPer1MTokens != want || r.CachedImageInputPer1MTokens == nil || *r.CachedImageInputPer1MTokens != 0 || r.ImageOutputPer1MTokens == nil || *r.ImageOutputPer1MTokens != 9.876543 {
			t.Fatalf("rates=%+v", r)
		}
	}
	check(1.234567)
	rr = do("PUT", "/data/pricing-templates/"+created.Template.ID, strings.Replace(body, "1.234567", "2.345678", 1))
	if rr.Code != 200 {
		t.Fatalf("update %d %s", rr.Code, rr.Body)
	}
	check(2.345678)
	// Legacy model-cost projection and upsert must carry the same dimensions.
	rr = do("PUT", "/data/model-costs/image-model", `{"image_input_per_1m_tokens":3.456789,"cached_image_input_per_1m_tokens":0,"image_output_per_1m_tokens":9.876543}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("model cost update %d %s", rr.Code, rr.Body)
	}
	check(3.456789)
	rr = do("GET", "/data/model-costs", "")
	if !strings.Contains(rr.Body.String(), `"image_input_per_1m_tokens":3.456789`) || !strings.Contains(rr.Body.String(), `"cached_image_input_per_1m_tokens":0`) {
		t.Fatalf("projection %s", rr.Body)
	}
	rr = do("POST", "/data/pricing-templates", strings.Replace(body, "1.234567", "-1", 1))
	if rr.Code != 400 {
		t.Fatalf("negative image price accepted %d", rr.Code)
	}
}
