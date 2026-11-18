package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"

	"github.com/go-chi/chi/v5"
)

// /admin/metrics exposes the per-(target, upstream model) performance
// estimates and the shadow routing comparison of performance-priority pools.
// Both are read-only and must be reachable with a readonly session.
func TestMetricsPerformanceAndShadowRouting(t *testing.T) {
	// A streaming response with two content events and final usage: each serve
	// feeds one TTFT and one TPS sample, so repeated requests make the pool's
	// estimates sufficient and the shadow comparison count.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(time.Millisecond)
		fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":40,\"total_tokens\":43}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	tempDir := t.TempDir()
	cfg := testConfig(tempDir, 0, nil)
	cfg.Targets = []config.Target{
		{Name: "perf-a", EndpointType: "openai", Endpoint: upstream.URL, APIKey: "up-key",
			UsageAdapter: config.UsageAdapterStandard, Pool: "perf",
			ModelMappings: []config.ModelMapping{{Upstream: "qwen-max"}}},
		{Name: "perf-b", EndpointType: "openai", Endpoint: upstream.URL, APIKey: "up-key",
			UsageAdapter: config.UsageAdapterStandard, Pool: "perf",
			ModelMappings: []config.ModelMapping{{Upstream: "qwen-max"}}},
	}
	cfg.Pools = []config.DeploymentPool{{Name: "perf", RoutingStrategy: config.PoolRoutingLoad}}
	configPath := filepath.Join(tempDir, "config.json")
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	// Leave the client IP unrestricted so each request can use a fresh
	// RemoteAddr: a shared affinity binding would turn later requests into
	// sticky hits, which the shadow deliberately does not count.
	clients := []config.Client{{Name: "client-1", AccessKey: "k1"}}
	if err := stores.clientStore.Create(clients[0]); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sessionManager := NewSessionManager(config.AdminSessionConfig{
		Secret: "metrics-performance-secret", CookieName: "metrics-performance-session", TTLSeconds: 3600,
	}, logger)
	clientAuth := auth.NewStore()
	if err := clientAuth.LoadFromConfig(clients); err != nil {
		t.Fatalf("client auth: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	h := NewHandler(config.NewManager(configPath), clientAuth, service, stores.auditStore, stores.userStore,
		stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessionManager, logger)

	router := chi.NewRouter()
	router.Use(sessionManager.Middleware)
	router.Mount("/", h)
	_, readonlyCookie, err := sessionManager.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("readonly session: %v", err)
	}

	readMetrics := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil)
		req.AddCookie(readonlyCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("readonly GET /metrics: HTTP %d %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /metrics: %v", err)
		}
		return body
	}
	runtimeEntry := func(body map[string]any, name string) map[string]any {
		t.Helper()
		entries, ok := body["targets_runtime"].([]any)
		if !ok {
			t.Fatalf("targets_runtime missing: %v", body["targets_runtime"])
		}
		for _, raw := range entries {
			entry := raw.(map[string]any)
			if entry["name"] == name {
				return entry
			}
		}
		t.Fatalf("target %q missing from targets_runtime", name)
		return nil
	}

	// Nothing recorded yet: both blocks are omitted, no null placeholders.
	body := readMetrics()
	if _, ok := body["shadow_routing"]; ok {
		t.Fatalf("shadow_routing present without statistics: %v", body["shadow_routing"])
	}
	for _, name := range []string{"perf-a", "perf-b"} {
		if _, ok := runtimeEntry(body, name)["model_performance"]; ok {
			t.Fatalf("model_performance present for %q without samples", name)
		}
	}

	// Warm the pool past perfMinSamples: 12 ordinary proxy requests with a
	// fresh client IP each. The last selections then have a sufficient member
	// and record one shadow comparison per selection.
	send := func(i int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer k1")
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
		rec := httptest.NewRecorder()
		auth.Middleware(clientAuth, logger)(service).ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 12; i++ {
		if rec := send(i); rec.Code != http.StatusOK {
			t.Fatalf("proxy request %d: HTTP %d %s", i, rec.Code, rec.Body.String())
		}
	}

	body = readMetrics()
	shadow, ok := body["shadow_routing"].([]any)
	if !ok || len(shadow) != 1 {
		t.Fatalf("shadow_routing = %v, want one pool", body["shadow_routing"])
	}
	pool := shadow[0].(map[string]any)
	if pool["pool"] != "perf" {
		t.Fatalf("shadow pool name = %v", pool["pool"])
	}
	agree, _ := pool["agree"].(float64)
	disagree, _ := pool["disagree"].(float64)
	total := agree + disagree
	if total < 1 {
		t.Fatalf("agree/disagree = %v/%v, want at least one comparison", pool["agree"], pool["disagree"])
	}
	members, ok := pool["members"].([]any)
	if !ok || len(members) != 2 {
		t.Fatalf("shadow members = %v, want both pool members", pool["members"])
	}
	shareSum := 0.0
	shadowSum := 0.0
	for _, raw := range members {
		member := raw.(map[string]any)
		if _, ok := member["target"].(string); !ok {
			t.Fatalf("member without target: %v", member)
		}
		shareSum += member["share"].(float64)
		shadowSum += member["shadow_selections"].(float64)
	}
	if shadowSum != total {
		t.Fatalf("member shadow selections sum = %v, want agree+disagree = %v", shadowSum, total)
	}
	if diff := shareSum - 1; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("member shares sum = %v, want 1", shareSum)
	}

	// Both members served the warmup, so both now expose their estimates.
	for _, name := range []string{"perf-a", "perf-b"} {
		served := runtimeEntry(body, name)["model_performance"]
		models, ok := served.([]any)
		if !ok || len(models) != 1 {
			t.Fatalf("%s model_performance = %v", name, served)
		}
		est := models[0].(map[string]any)
		if est["target"] != name || est["upstream_model"] != "qwen-max" {
			t.Fatalf("%s model performance entry = %v", name, est)
		}
		if _, err := time.Parse(time.RFC3339Nano, est["updated_at"].(string)); err != nil {
			t.Fatalf("updated_at = %v: %v", est["updated_at"], err)
		}
		for _, key := range []string{"ttft", "tps", "penalty_ttft"} {
			estPart, ok := est[key].(map[string]any)
			if !ok {
				t.Fatalf("%s %s = %v", name, key, est[key])
			}
			for _, nested := range []string{"p50", "p90", "samples", "insufficient"} {
				if _, ok := estPart[nested]; !ok {
					t.Fatalf("%s %s lost key %q", name, key, nested)
				}
			}
		}
		ttft := est["ttft"].(map[string]any)
		if ttft["samples"].(float64) < 1 || ttft["insufficient"] != false {
			t.Fatalf("%s ttft = %v", name, est["ttft"])
		}
		if _, ok := est["non_stream"].(map[string]any); !ok {
			t.Fatalf("%s non_stream = %v", name, est["non_stream"])
		}
		// No model was ever dropped: the counter stays omitted.
		if _, ok := runtimeEntry(body, name)["dropped_performance_models"]; ok {
			t.Fatalf("%s unexpectedly reports dropped_performance_models", name)
		}
	}
}

// A target whose model table is full of live entries reports how many new
// upstream models it dropped in targets_runtime[].
func TestMetricsDroppedPerformanceModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":40,\"total_tokens\":43}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	// 33 distinct upstream models on one target: the bounded table holds 32 and
	// must count the last one as dropped while it is still fresh.
	const models = 33
	mappings := make([]config.ModelMapping, 0, models)
	for i := 0; i < models; i++ {
		mappings = append(mappings, config.ModelMapping{Upstream: fmt.Sprintf("perf-model-%02d", i)})
	}
	tempDir := t.TempDir()
	cfg := testConfig(tempDir, 0, nil)
	cfg.Targets = []config.Target{{
		Name: "perf-drop", EndpointType: "openai", Endpoint: upstream.URL, APIKey: "up-key",
		UsageAdapter: config.UsageAdapterStandard, ModelMappings: mappings,
	}}
	configPath := filepath.Join(tempDir, "config.json")
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	clients := []config.Client{{Name: "client-1", AccessKey: "k1"}}
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sessionManager := NewSessionManager(config.AdminSessionConfig{
		Secret: "metrics-dropped-secret", CookieName: "metrics-dropped-session", TTLSeconds: 3600,
	}, logger)
	clientAuth := auth.NewStore()
	if err := clientAuth.LoadFromConfig(clients); err != nil {
		t.Fatalf("client auth: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	h := NewHandler(config.NewManager(configPath), clientAuth, service, stores.auditStore, stores.userStore,
		stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessionManager, logger)

	router := chi.NewRouter()
	router.Use(sessionManager.Middleware)
	router.Mount("/", h)
	_, readonlyCookie, err := sessionManager.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("readonly session: %v", err)
	}

	for i := 0; i < models; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions",
			strings.NewReader(fmt.Sprintf(`{"model":"perf-model-%02d","stream":true,"messages":[{"role":"user","content":"hi"}]}`, i)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer k1")
		rec := httptest.NewRecorder()
		auth.Middleware(clientAuth, logger)(service).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy request %d: HTTP %d %s", i, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil)
	req.AddCookie(readonlyCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readonly GET /metrics: HTTP %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	entries, ok := body["targets_runtime"].([]any)
	if !ok {
		t.Fatalf("targets_runtime missing: %v", body["targets_runtime"])
	}
	for _, raw := range entries {
		entry := raw.(map[string]any)
		if entry["name"] != "perf-drop" {
			continue
		}
		if got, _ := entry["dropped_performance_models"].(float64); got != 1 {
			t.Fatalf("dropped_performance_models = %v, want 1", entry["dropped_performance_models"])
		}
		served, ok := entry["model_performance"].([]any)
		if !ok || len(served) != models-1 {
			t.Fatalf("model_performance entries = %d, want %d", len(served), models-1)
		}
		return
	}
	t.Fatal("target perf-drop missing from targets_runtime")
}
