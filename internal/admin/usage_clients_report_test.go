package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

func TestUsageByClientRoute(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 0, nil)
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessionManager := NewSessionManager(config.AdminSessionConfig{
		Secret: "usage-by-client-secret", CookieName: "usage-by-client-session", TTLSeconds: 3600,
	}, logger)
	manager := config.NewManager(configPath)
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}
	h := NewHandler(manager, auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore,
		stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessionManager, logger)
	h.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil
	}, nil)
	router := chi.NewRouter()
	router.Use(sessionManager.Middleware)
	router.Mount("/", h)

	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)
	for _, event := range []usage.Event{
		{Timestamp: day.Add(10*time.Hour + 5*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
		{Timestamp: day.Add(10*time.Hour + 6*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 500},
		{Timestamp: day.Add(11*time.Hour + 6*time.Minute), ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000, StatusCode: 200},
	} {
		if err := stores.usageStore.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	query := "?from=" + url.QueryEscape(day.Format(time.RFC3339)) + "&to=" + url.QueryEscape(day.Add(24*time.Hour).Format(time.RFC3339))
	_, readonlyCookie, err := sessionManager.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/by-client"+query, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET by-client: got %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/by-client"+query, nil)
	req.AddCookie(readonlyCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readonly GET by-client: got %d, body=%s", rec.Code, rec.Body.String())
	}
	firstBody := rec.Body.String()
	var summary usage.ClientUsageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Exact || summary.Source != usage.ClientUsageSourceAggregate {
		t.Fatalf("summary exact=%v source=%q, want aggregate", summary.Exact, summary.Source)
	}
	want := []usage.DimensionTotals{
		{Key: "alice", Totals: usage.Totals{Requests: 2, InputTokens: 2_000_000, EstimatedCost: 4}},
		{Key: "bob", Totals: usage.Totals{Requests: 1, InputTokens: 2_000_000, EstimatedCost: 4}},
	}
	if !reflect.DeepEqual(summary.Clients, want) {
		t.Fatalf("clients=%+v want %+v", summary.Clients, want)
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/aggregate"+query+"&group_by=day", nil)
	req.AddCookie(readonlyCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate GET: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var aggregate usage.AggregateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &aggregate); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Clients, aggregate.ByClient) {
		t.Fatalf("by-client report differs from aggregate by_client:\n got=%+v\nwant=%+v", summary.Clients, aggregate.ByClient)
	}

	// The route is wrapped in the report cache: within the TTL the stored body
	// is served even if a later read would fail, and expiry recomputes.
	if err := stores.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketMeta)).Put([]byte("usage_aggregate_source_missing_v1:"+nosql.BucketQuotaUsageAggHourly), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/by-client"+query, nil)
	req.AddCookie(readonlyCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != firstBody {
		t.Fatalf("cached by-client response: status=%d body=%s", rec.Code, rec.Body.String())
	}
	realNow := time.Now()
	clock := &fakeClock{t: realNow}
	h.reportCache.now = clock.Now
	clock.Advance(2 * time.Minute)
	req = httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/by-client"+query, nil)
	req.AddCookie(readonlyCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expired cache should recompute and report the missing source: got %d, body=%s", rec.Code, rec.Body.String())
	}
}
