package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/costutil"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/go-chi/chi/v5"
)

// testStores creates a bbolt DB and all stores for testing.
type testStores struct {
	clientStore      *nosql.ClientStore
	clientQuotaStore *nosql.ClientQuotaStore
	modelGroupStore  *nosql.ModelGroupStore
	modelCostStore   *nosql.ModelCostStore
	usageStore       *nosql.UsageStore
	userStore        *nosql.UserStore
	auditStore       *nosql.AuditStore
	targetStore      *nosql.TargetStore
}

type permanentUsageFailureRecorder struct{}

func (permanentUsageFailureRecorder) Record(usage.Event) error {
	return fmt.Errorf("primary store and recovery journal unavailable")
}

func setupTestStores(t *testing.T, tempDir string) testStores {
	t.Helper()
	dbPath := filepath.Join(tempDir, "test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return testStores{
		clientStore:      nosql.NewClientStore(db),
		clientQuotaStore: nosql.NewClientQuotaStore(db),
		modelGroupStore:  nosql.NewModelGroupStore(db),
		modelCostStore:   nosql.NewModelCostStore(db),
		usageStore:       nosql.NewUsageStore(db),
		userStore:        nosql.NewUserStore(db),
		auditStore:       nosql.NewAuditStore(db),
		targetStore:      nosql.NewTargetStore(db),
	}
}

func seedClients(t *testing.T, store *nosql.ClientStore, clients []config.Client) {
	t.Helper()
	for _, c := range clients {
		if err := store.Create(c); err != nil {
			t.Fatalf("failed to seed client %q: %v", c.Name, err)
		}
	}
}

func TestHandlerUIEntry(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	for _, route := range []string{"/", "/ui"} {
		t.Run(route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com"+route, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("ui entry expected 200, got %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("expected text/html content-type, got %q", ct)
			}

			body := rec.Body.String()
			if !strings.Contains(body, "客户端管理") || !strings.Contains(body, "消费统计") {
				t.Fatalf("ui page missing expected tab labels")
			}
			if !strings.Contains(body, `/admin/ui/js/clients.js`) || !strings.Contains(body, `/admin/ui/js/usage.js`) || !strings.Contains(adminUIAllScripts(), "/admin/data/clients") || !strings.Contains(adminUIAllScripts(), "/admin/data/usage/aggregate") {
				t.Fatalf("ui page missing expected admin api endpoints")
			}
		})

		// 静态资源：额度管理 JS 已拆分为独立文件（见 ui/js/quota.js）
		t.Run("quota js asset", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/ui/js/quota.js", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("quota.js asset expected 200, got %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/javascript") {
				t.Fatalf("expected text/javascript content-type, got %q", ct)
			}
		})
	}
}

func TestHandlerUIEntryWhenMountedUnderAuth(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	adminHandler := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	protected := chi.NewRouter()
	protected.Use(auth.Middleware(store, logger))
	protected.Mount("/admin", adminHandler)

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/admin/ui", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	withAuth := httptest.NewRequest(http.MethodGet, "http://example.com/admin/ui", nil)
	withAuth.Header.Set("Authorization", "Bearer k1")
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, withAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", rec.Code)
	}
}

func TestHandlerHealthz(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 2, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		Status      string               `json:"status"`
		Targets     []proxy.TargetStatus `json:"targets"`
		TargetCount int                  `json:"target_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status == "" {
		t.Fatal("expected status field to be populated")
	}
	if resp.TargetCount != 2 {
		t.Fatalf("expected target_count 2, got %d", resp.TargetCount)
	}
	if len(resp.Targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(resp.Targets))
	}
}

func TestHandlerMetrics(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		UptimeSeconds float64          `json:"uptime_seconds"`
		Targets       int              `json:"targets"`
		Requests      map[string]int64 `json:"requests"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.UptimeSeconds < 0 {
		t.Fatalf("expected non-negative uptime, got %f", resp.UptimeSeconds)
	}
	if resp.Targets != 1 {
		t.Fatalf("expected targets 1, got %d", resp.Targets)
	}
	if count, ok := resp.Requests["usage_record_failures"]; !ok || count != 0 {
		t.Fatalf("usage_record_failures metric = %d (present=%v), want present and 0", count, ok)
	}
}

func TestHandlerReloadConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initialClients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, initial)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, initialClients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	// Update config with 2 targets and new client.
	// Also update the bbolt client store to have k2 instead of k1.
	updated := testConfig(tempDir, 2, []string{"k2"})
	writeConfigFile(t, configPath, updated)
	// Delete old client, add new one in bbolt.
	_ = stores.clientStore.Delete("client-1")
	updatedClients := testClients([]string{"k2"})
	seedClients(t, stores.clientStore, updatedClients)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status  string `json:"status"`
		Targets int    `json:"targets"`
		Clients int    `json:"clients"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}
	if resp.Targets != 2 || resp.Clients != 1 {
		t.Fatalf("unexpected counts: targets=%d clients=%d", resp.Targets, resp.Clients)
	}

	current, err := manager.Current()
	if err != nil {
		t.Fatalf("expected manager.Current to succeed, got %v", err)
	}
	if len(current.Targets) != 2 {
		t.Fatalf("expected manager cache to be updated to 2 targets, got %d", len(current.Targets))
	}

	if _, ok := store.Authenticate("k2"); !ok {
		t.Fatal("expected auth store to contain updated client key k2")
	}
	if _, ok := store.Authenticate("k1"); ok {
		t.Fatal("expected auth store to drop old client key k1")
	}
}

func TestHandlerReloadConfigRejectsInvalidProxyConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initialClients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, initial)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, initialClients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	manager.Replace(initial)

	store := auth.NewStore()
	if err := store.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}

	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	invalid := testConfig(tempDir, 1, []string{"k2"})
	invalid.Targets[0].Endpoint = "not-a-valid-url"
	writeConfigFile(t, configPath, invalid)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	if _, ok := store.Authenticate("k1"); !ok {
		t.Fatal("expected original key k1 to remain after rejected reload")
	}

	current, err := manager.Current()
	if err != nil {
		t.Fatalf("expected manager.Current to return cached config, got %v", err)
	}
	if len(current.Targets) != 1 || current.Targets[0].Endpoint != initial.Targets[0].Endpoint {
		t.Fatalf("expected manager cache to remain unchanged, got %#v", current.Targets)
	}
}

func TestHandlerDataClientsCRUD(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/clients", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list clients expected 200, got %d", rec.Code)
	}

	body := bytes.NewBufferString(`{"name":"client-2","access_key":"k2","ip_address":"192.0.2.0/24, 10.0.1.1","allowed_targets":[]}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k2"); !ok {
		t.Fatalf("expected auth store updated with k2")
	}
	storedClients, err := stores.clientStore.List()
	if err != nil {
		t.Fatalf("list clients after create: %v", err)
	}
	var storedClient config.Client
	for _, client := range storedClients {
		if client.Name == "client-2" {
			storedClient = client
			break
		}
	}
	if storedClient.IPAddress != "192.0.2.0/24,10.0.1.1" {
		t.Fatalf("expected client IPv4/CIDR list persisted, got %+v", storedClient)
	}

	body = bytes.NewBufferString(`{"name":"client-2","access_key":"k3","ip_address":"198.51.100.2 203.0.113.0/24","allowed_targets":[]}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/clients/client-2", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("update client expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k3"); !ok {
		t.Fatalf("expected auth store updated with k3")
	}
	storedClients, err = stores.clientStore.List()
	if err != nil {
		t.Fatalf("list clients after update: %v", err)
	}
	for _, client := range storedClients {
		if client.Name == "client-2" && client.IPAddress != "198.51.100.2,203.0.113.0/24" {
			t.Fatalf("expected updated client IPv4/CIDR list persisted, got %+v", client)
		}
	}
	if principal, ok := store.Authenticate("k3"); !ok || !principal.CanAccessIP(netip.MustParseAddr("203.0.113.42")) || principal.CanAccessIP(netip.MustParseAddr("203.0.114.42")) {
		t.Fatalf("auth store did not apply updated client IP rules")
	}
	body = bytes.NewBufferString(`{"name":"invalid-ip","access_key":"k-invalid","ip_address":"192.0.2.0/33"}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid CIDR expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/clients/client-1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete client expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k1"); ok {
		t.Fatalf("expected client-1 removed from auth store")
	}
}

func TestHandlerUpdateTargetPreservesKeyPoolWithExistingRefs(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := map[string]any{
		"endpoint_type":            "azure_openai",
		"endpoint":                 "https://example.com",
		"resource_path_prefix":     "/openai",
		"api_key":                  "__existing_key_index__:0",
		"api_keys":                 []string{"__existing_key_index__:1", "__existing_key_index__:2"},
		"allow_bearer_passthrough": false,
		"model_mappings":           []map[string]any{{"upstream": "gpt-4o"}, {"upstream": "gpt-4o-mini"}},
	}
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.APIKey != initialKeys[0] {
		t.Fatalf("APIKey overwritten: got %q want %q", updated.APIKey, initialKeys[0])
	}
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys changed: got %#v want %#v", got, want)
	}
	if got := modelMappingsUpstreams(updated.ModelMappings); got != "gpt-4o,gpt-4o-mini" {
		t.Fatalf("model_mappings upstream names not updated, got %q", got)
	}
}

// modelMappingsUpstreams joins the upstream names from ModelMappings into a
// comma-separated string, mirroring the old AllowedModels join format for test
// assertions.
func modelMappingsUpstreams(mappings []config.ModelMapping) string {
	names := make([]string, 0, len(mappings))
	for _, m := range mappings {
		names = append(names, m.Upstream)
	}
	return strings.Join(names, ",")
}

func TestHandlerListTargetsReturnsPaused(t *testing.T) {
	tempDir := t.TempDir()
	h, _, _ := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets []struct {
			Name   string `json:"name"`
			Paused bool   `json:"paused"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Name != "target-1" {
		t.Fatalf("unexpected targets: %#v", resp.Targets)
	}
}

func TestHandlerListAndUpdateTargetPreservesImageOperation(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})
	initial := config.Target{
		UsageAdapter:   config.UsageAdapterStandard,
		Name:           "target-1",
		EndpointType:   config.EndpointTypeOpenAIImage,
		Endpoint:       "https://image.example.com/v1/images/edits",
		APIKey:         "sk-primary-real-0001",
		ImageOperation: config.ImageOperationEdits,
		ModelMappings:  []config.ModelMapping{{Upstream: "gpt-image-2"}},
	}
	if err := stores.targetStore.Create(initial); err != nil {
		t.Fatalf("seed image operation target: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets []struct {
			Name           string `json:"name"`
			EndpointType   string `json:"endpoint_type"`
			ImageOperation string `json:"image_operation"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].EndpointType != config.EndpointTypeOpenAIImage || resp.Targets[0].ImageOperation != config.ImageOperationEdits {
		t.Fatalf("list did not include image_operation: %#v", resp.Targets)
	}

	body := map[string]any{
		"endpoint_type":            config.EndpointTypeOpenAIImage,
		"endpoint":                 "https://image.example.com/v1/images/edits",
		"image_operation":          config.ImageOperationEdits,
		"api_key":                  "__existing_key_index__:0",
		"allow_bearer_passthrough": false,
		"model_mappings":           []map[string]any{{"upstream": "gpt-image-2"}},
	}
	rec = putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.ImageOperation != config.ImageOperationEdits {
		t.Fatalf("image_operation not preserved after update: %+v", updated)
	}
}

func TestHandlerUpdateTargetSetsPausedInStoreAndRuntime(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"})
	body["paused"] = true
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if !updated.Paused {
		t.Fatalf("expected paused=true in target store")
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true, got %#v", statuses)
	}

	body["paused"] = false
	rec = putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated = mustGetTarget(t, stores.targetStore, "target-1")
	if updated.Paused {
		t.Fatalf("expected paused=false in target store")
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false, got %#v", statuses)
	}
}

func TestHandlerPauseResumeTargetPreservesAPIKeys(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, tempDir, initialKeys)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1/pause", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	paused := mustGetTarget(t, stores.targetStore, "target-1")
	if !paused.Paused {
		t.Fatalf("expected paused=true")
	}
	if paused.APIKey != initialKeys[0] || fmt.Sprint(paused.APIKeys) != fmt.Sprint(initialKeys[1:]) {
		t.Fatalf("keys changed on pause: %#v", paused)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true, got %#v", statuses)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resumed := mustGetTarget(t, stores.targetStore, "target-1")
	if resumed.Paused {
		t.Fatalf("expected paused=false")
	}
	if resumed.APIKey != initialKeys[0] || fmt.Sprint(resumed.APIKeys) != fmt.Sprint(initialKeys[1:]) {
		t.Fatalf("keys changed on resume: %#v", resumed)
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false, got %#v", statuses)
	}
}

func TestHandlerPauseResumeURLUnescapesChineseTargetName(t *testing.T) {
	tempDir := t.TempDir()
	name := "网宿-Gemini"
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, stores, service := newNamedTargetUpdateTestHandler(t, tempDir, name, initialKeys)
	escapedName := url.PathEscape(name)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+escapedName+"/pause", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause Chinese target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	paused := mustGetTarget(t, stores.targetStore, name)
	if !paused.Paused {
		t.Fatalf("expected paused=true for %q", name)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Name != name || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true for %q, got %#v", name, statuses)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+escapedName+"/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume Chinese target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resumed := mustGetTarget(t, stores.targetStore, name)
	if resumed.Paused {
		t.Fatalf("expected paused=false for %q", name)
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Name != name || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false for %q, got %#v", name, statuses)
	}
}

func TestHandlerUpdateTargetAcceptsExactMaskedExistingKeys(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody(initialKeys[0], []string{maskKey(initialKeys[1]), maskKey(initialKeys[2])})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.APIKey != initialKeys[0] {
		t.Fatalf("APIKey overwritten: got %q want %q", updated.APIKey, initialKeys[0])
	}
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys changed: got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetDeletesOmittedSecondaryKey(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:2"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, []string{initialKeys[2]}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys after delete got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetKeepsExistingSecondaryAndAddsNewOne(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1", "sk-secondary-real-0004"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, []string{initialKeys[1], "sk-secondary-real-0004"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys after add got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetRejectsUnrecognizedMaskedLookingKey(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"sk-b...dead"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update target expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys should remain unchanged after reject, got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetRejectsOutOfRangeExistingRef(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:9"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update target expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys should remain unchanged after reject, got %#v want %#v", got, want)
	}
}

func newTargetUpdateTestHandler(t *testing.T, tempDir string, keys []string) (http.Handler, *config.Manager, testStores) {
	h, manager, stores, _ := newTargetUpdateTestHandlerWithService(t, tempDir, keys)
	return h, manager, stores
}

func newTargetUpdateTestHandlerWithService(t *testing.T, tempDir string, keys []string) (http.Handler, *config.Manager, testStores, *proxy.Service) {
	t.Helper()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.Targets[0].APIKey = keys[0]
	if len(keys) > 1 {
		cfg.Targets[0].APIKeys = append([]string(nil), keys[1:]...)
	}
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	return h, manager, stores, service
}

func newNamedTargetUpdateTestHandler(t *testing.T, tempDir, name string, keys []string) (http.Handler, testStores, *proxy.Service) {
	t.Helper()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.Targets[0].Name = name
	cfg.Targets[0].APIKey = keys[0]
	if len(keys) > 1 {
		cfg.Targets[0].APIKeys = append([]string(nil), keys[1:]...)
	}
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	return h, stores, service
}

func targetUpdateBody(apiKey string, apiKeys []string) map[string]any {
	return map[string]any{
		"endpoint_type":            "azure_openai",
		"endpoint":                 "https://example.com",
		"resource_path_prefix":     "/openai",
		"api_key":                  apiKey,
		"api_keys":                 apiKeys,
		"allow_bearer_passthrough": false,
		"model_mappings":           []map[string]any{{"upstream": "gpt-4o"}},
	}
}

func putTarget(t *testing.T, h http.Handler, name string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal update body: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+name, bytes.NewReader(payload)))
	return rec
}

func mustGetTarget(t *testing.T, store *nosql.TargetStore, name string) config.Target {
	t.Helper()
	targets, err := store.List()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	for _, target := range targets {
		if target.Name == name {
			return target
		}
	}
	t.Fatalf("target %q not found", name)
	return config.Target{}
}

func TestHandlerUsageSummary(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	// Record a usage event via bbolt store.
	now := time.Now().UTC()
	if err := stores.usageStore.Record(usage.Event{Timestamp: now.Add(-10 * time.Minute), ClientName: "client-1", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50}); err != nil {
		t.Fatalf("record usage event: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	body := bytes.NewBufferString(`{"input_per_1m_tokens":10,"output_per_1m_tokens":20,"cache_write_per_1m_tokens":0}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/model-costs/gpt-4o", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert model cost expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		LastHour struct {
			Requests int64 `json:"requests"`
		} `json:"last_hour"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode summary response: %v", err)
	}
	if payload.LastHour.Requests == 0 {
		t.Fatalf("expected summary last_hour requests > 0")
	}
}

func writeConfigFile(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
}

func testConfig(tempDir string, targetCount int, clientKeys []string) *config.Config {
	_ = clientKeys
	targets := make([]config.Target, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		targets = append(targets, config.Target{
			UsageAdapter:       config.UsageAdapterStandard,
			Name:               fmt.Sprintf("target-%d", i+1),
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings:      []config.ModelMapping{{Upstream: "gpt-4o"}},
		})
	}

	return &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: targets,
		DataStore: config.DataStore{
			DBPath: filepath.Join(tempDir, "test.db"),
		},
		AdminSession: config.AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: filepath.Join(tempDir, "logs", "access.log"),
			ErrorLog:  filepath.Join(tempDir, "logs", "error.log"),
		},
	}
}

func testClients(clientKeys []string) []config.Client {
	clients := make([]config.Client, 0, len(clientKeys))
	for i, key := range clientKeys {
		clients = append(clients, config.Client{
			Name:      fmt.Sprintf("client-%d", i+1),
			AccessKey: key,
			IPAddress: fmt.Sprintf("192.0.2.%d", i+1),
		})
	}
	return clients
}

func TestToUsageCostTableCatalogFallback(t *testing.T) {
	// Build a small catalog for testing.
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New() error: %v", err)
	}

	// Scenario 1: catalog only (no custom costs) — should use catalog default_cost
	table := costutil.ToCostTable(nil, cat)
	rate, ok := table.LookupCost("gpt-4o")
	if !ok {
		t.Fatal("expected catalog default cost for gpt-4o, got not found")
	}
	if rate.InputPer1MTokens <= 0 || rate.OutputPer1MTokens <= 0 {
		t.Fatalf("expected positive catalog default rates, got %+v", rate)
	}
	catalogInput := rate.InputPer1MTokens

	// Scenario 2: custom override — should override catalog defaults
	customCosts := []nosql.ModelCost{
		{Model: "gpt-4o", InputPer1MTokens: 999, OutputPer1MTokens: 888},
	}
	table = costutil.ToCostTable(customCosts, cat)
	rate, ok = table.LookupCost("gpt-4o")
	if !ok {
		t.Fatal("expected cost for gpt-4o after custom override")
	}
	if rate.InputPer1MTokens != 999 || rate.OutputPer1MTokens != 888 {
		t.Fatalf("expected custom rates (999, 888), got %+v", rate)
	}

	// Scenario 3: catalog without custom — other models still use catalog defaults
	rate2, ok := table.LookupCost("gpt-4o-mini")
	if !ok {
		t.Fatal("expected catalog default cost for gpt-4o-mini (not overridden)")
	}
	if rate2.InputPer1MTokens <= 0 {
		t.Fatalf("expected positive catalog default for gpt-4o-mini, got %+v", rate2)
	}

	// Scenario 4: nil catalog — only custom costs
	table = costutil.ToCostTable(customCosts, nil)
	rate, ok = table.LookupCost("gpt-4o")
	if !ok || rate.InputPer1MTokens != 999 {
		t.Fatalf("expected custom rate with nil catalog, got ok=%v rate=%+v", ok, rate)
	}
	_, ok = table.LookupCost("gpt-4o-mini")
	if ok {
		t.Fatal("expected no cost for gpt-4o-mini with nil catalog and no custom entry")
	}

	// Confirm catalog default is different from custom value
	if catalogInput == 999 {
		t.Fatal("test setup issue: catalog default same as custom override value")
	}
}

func TestHandlerEndpointTypesEndpoint(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/endpoint-types", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Count         int                       `json:"count"`
		EndpointTypes []config.EndpointTypeMeta `json:"endpoint_types"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count == 0 || len(resp.EndpointTypes) == 0 {
		t.Fatal("expected non-empty endpoint types")
	}
	if resp.Count != len(resp.EndpointTypes) {
		t.Fatalf("count(%d) != len(%d)", resp.Count, len(resp.EndpointTypes))
	}
	// 必须包含 openai_image 类型
	want := map[string]bool{"openai_image": false}
	for _, m := range resp.EndpointTypes {
		if _, ok := want[m.Code]; ok {
			want[m.Code] = true
			if m.DisplayName == "" || m.ShortLabel == "" {
				t.Errorf("%s 缺少 DisplayName 或 ShortLabel", m.Code)
			}
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("API 未返回 %s", code)
		}
	}
}

// ---------- Quota Status Endpoint ----------

// TestHandlerQuotaStatus_QuotaManagerNil_503 验证 quotaManager 未配置时返回 503。
func TestHandlerQuotaStatus_QuotaManagerNil_503(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	// 请求无 quotaManager 时应返回 503
	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/quota/k1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d. body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v. body=%s", err, rec.Body.String())
	}
	if !strings.Contains(body.Error, "quota") {
		t.Errorf("expected error mention 'quota', got %q", body.Error)
	}
}

// TestHandlerQuotaStatus_ClientNotFound_ReturnsEmpty 验证未知 client 返回空 Status JSON（非 404）。
func TestHandlerQuotaStatus_ClientNotFound_ReturnsEmpty(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	// 创建一个空 quota manager（client store 为空 → Status 返回空结构）
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cat, _ := catalog.New()
	qm, err := quota.New(quota.Options{
		Catalog:     cat,
		ClientStore: stores.clientStore,
		UsageStore:  stores.usageStore,
		CostStore:   stores.modelCostStore,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}

	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, qm, nil, nil, logger)

	// 不存在的 client：返回 200 + 空 quotas（QuotaStatus 中 Exceeded=nil）
	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/quota/unknown", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d. body=%s", rec.Code, rec.Body.String())
	}

	var status quota.QuotaStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v. body=%s", err, rec.Body.String())
	}
	if status.Client != "unknown" {
		t.Errorf("expected client 'unknown', got %q", status.Client)
	}
	if status.Exceeded != nil {
		t.Errorf("expected Exceeded=nil for unknown client, got %+v", status.Exceeded)
	}
}

// coefPtrAdmin 返回 plan 个人系数指针（nil=默认1，0=不限，>0=乘数）。
func coefPtrAdmin(v float64) *float64 { return &v }

func TestHandlerQuotaSummaryDoesNotExposeClientCredentials(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"summary-secret"})
	clients := []config.Client{{
		Name:            "alice",
		AccessKey:       "summary-secret",
		IPAddress:       "192.0.2.10",
		AllowedTargets:  []string{"private-target"},
		JobLevel:        "L6",
		ProjectGroup:    "platform",
		QuotaDailyUSD:   10,
		QuotaWeeklyUSD:  20,
		QuotaMonthlyUSD: 30,
	}}
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	if err := stores.targetStore.Create(config.Target{
		UsageAdapter:  config.UsageAdapterStandard,
		Name:          "private-target",
		EndpointType:  "openai",
		Endpoint:      "https://example.com",
		APIKey:        "key",
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
	}); err != nil {
		t.Fatalf("failed to seed target: %v", err)
	}
	seedClients(t, stores.clientStore, clients)
	if err := stores.modelGroupStore.Create(config.ModelGroup{
		Name:            "primary",
		CostCoefficient: 1,
		Bindings:        []config.QuotaGroupBinding{{Target: "private-target"}},
	}); err != nil {
		t.Fatalf("failed to seed model group: %v", err)
	}
	if err := stores.clientQuotaStore.Replace("alice", []config.QuotaGroup{{
		Name:        "primary",
		Coefficient: coefPtrAdmin(0.5),
	}}); err != nil {
		t.Fatalf("failed to seed client quota groups: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	qm, err := quota.New(quota.Options{Catalog: cat, ClientStore: stores.clientStore, ClientQuotaStore: stores.clientQuotaStore, ModelGroupStore: stores.modelGroupStore, UsageStore: stores.usageStore, CostStore: stores.modelCostStore, Logger: logger})
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, qm, nil, nil, logger)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/quota/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d. body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "access_key") || strings.Contains(body, "allowed_targets") || strings.Contains(body, "summary-secret") {
		t.Fatalf("quota summary exposed credential or unrelated auth data: %s", body)
	}

	var response struct {
		Clients []struct {
			Client struct {
				Name         string              `json:"name"`
				IPAddress    string              `json:"ip_address"`
				JobLevel     string              `json:"job_level"`
				ProjectGroup string              `json:"project_group"`
				QuotaGroups  []config.QuotaGroup `json:"quota_groups"`
			} `json:"client"`
			Status quota.QuotaStatus `json:"status"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v. body=%s", err, body)
	}
	if len(response.Clients) != 1 {
		t.Fatalf("expected one client, got %d", len(response.Clients))
	}
	got := response.Clients[0]
	if got.Client.Name != "alice" || got.Client.IPAddress != "192.0.2.10" || got.Client.JobLevel != "L6" || got.Client.ProjectGroup != "platform" {
		t.Errorf("missing client display fields: %+v", got.Client)
	}
	if len(got.Client.QuotaGroups) != 1 || got.Client.QuotaGroups[0].PlanCoefficient() != 0.5 {
		t.Errorf("missing plan configuration: %+v", got.Client)
	}
	if got.Status.Client != "alice" {
		t.Errorf("missing quota status, got %+v", got.Status)
	}
}

func TestHandlerClientDeleteArchivesQuotaEntry(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"archive-secret"})
	clients := []config.Client{{
		Name:            "bob",
		AccessKey:       "archive-secret",
		IPAddress:       "192.0.2.20",
		AllowedTargets:  []string{"private-target"},
		JobLevel:        "L5",
		ProjectGroup:    "AI",
		QuotaDailyUSD:   10,
		QuotaMonthlyUSD: 30,
	}}
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	if err := stores.targetStore.Create(config.Target{
		UsageAdapter:  config.UsageAdapterStandard,
		Name:          "private-target",
		EndpointType:  "openai",
		Endpoint:      "https://example.com",
		APIKey:        "key",
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
	}); err != nil {
		t.Fatalf("failed to seed target: %v", err)
	}
	if err := stores.modelGroupStore.Create(config.ModelGroup{
		Name:            "premium",
		CostCoefficient: 1.5,
		DailyUSD:        5,
		Bindings:        []config.QuotaGroupBinding{{Target: "private-target"}},
	}); err != nil {
		t.Fatalf("failed to seed model group: %v", err)
	}
	seedClients(t, stores.clientStore, clients)
	if err := stores.clientQuotaStore.Replace("bob", []config.QuotaGroup{{
		Name: "premium", Coefficient: coefPtrAdmin(0.5),
	}}); err != nil {
		t.Fatalf("failed to seed client quota groups: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	qm, err := quota.New(quota.Options{Catalog: cat, ClientStore: stores.clientStore, ClientQuotaStore: stores.clientQuotaStore, ModelGroupStore: stores.modelGroupStore, UsageStore: stores.usageStore, CostStore: stores.modelCostStore, Logger: logger})
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, qm, nil, nil, logger)

	type summaryRow struct {
		Client struct {
			Name         string              `json:"name"`
			IPAddress    string              `json:"ip_address"`
			JobLevel     string              `json:"job_level"`
			ProjectGroup string              `json:"project_group"`
			Deleted      bool                `json:"deleted"`
			QuotaGroups  []config.QuotaGroup `json:"quota_groups"`
		} `json:"client"`
	}
	decodeSummary := func(t *testing.T, body []byte) []summaryRow {
		t.Helper()
		var response struct {
			Clients []summaryRow `json:"clients"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("decode summary: %v. body=%s", err, string(body))
		}
		return response.Clients
	}

	// Deleting an active client's quota entry directly is rejected.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/quota/bob", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for active client cleanup, got %d. body=%s", rec.Code, rec.Body.String())
	}

	// Delete the client: quota entry must be archived, not removed.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/clients/bob", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d. body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/quota/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary failed: %d. body=%s", rec.Code, rec.Body.String())
	}
	rows := decodeSummary(t, rec.Body.Bytes())
	if len(rows) != 1 {
		t.Fatalf("expected archived row, got %d rows: %s", len(rows), rec.Body.String())
	}
	row := rows[0].Client
	if row.Name != "bob" || row.IPAddress != "192.0.2.20" || row.JobLevel != "L5" || row.ProjectGroup != "AI" {
		t.Errorf("archive snapshot missing identity fields: %+v", row)
	}
	if !row.Deleted {
		t.Errorf("expected deleted=true, got %+v", row)
	}
	if len(row.QuotaGroups) != 1 || row.QuotaGroups[0].Name != "premium" {
		t.Errorf("quota config not preserved: %+v", row.QuotaGroups)
	}

	// Purge the archived entry; the row disappears from the summary.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/quota/bob", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for archived cleanup, got %d. body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/quota/summary", nil))
	rows = decodeSummary(t, rec.Body.Bytes())
	if len(rows) != 0 {
		t.Fatalf("expected no rows after purge, got %s", rec.Body.String())
	}
}

func TestHandlerQuotaSyncAtomic(t *testing.T) {
	clientByName := func(t *testing.T, store *nosql.ClientStore, name string) config.Client {
		t.Helper()
		clients, err := store.List()
		if err != nil {
			t.Fatalf("list clients: %v", err)
		}
		for _, client := range clients {
			if client.Name == name {
				return client
			}
		}
		t.Fatalf("client %q not found", name)
		return config.Client{}
	}

	t.Run("unknown target leaves all targets unchanged", func(t *testing.T) {
		stores := setupTestStores(t, t.TempDir())
		seedClients(t, stores.clientStore, []config.Client{
			{Name: "source", AccessKey: "source-key", IPAddress: "192.0.2.1"},
			{Name: "first", AccessKey: "first-key", IPAddress: "192.0.2.2"},
			{Name: "second", AccessKey: "second-key", IPAddress: "192.0.2.3"},
		})
		if err := stores.modelGroupStore.Create(config.ModelGroup{Name: "shared", CostCoefficient: 1}); err != nil {
			t.Fatalf("seed model group: %v", err)
		}
		if err := stores.clientQuotaStore.Replace("first", []config.QuotaGroup{{Name: "shared", Coefficient: coefPtrAdmin(2)}}); err != nil {
			t.Fatalf("seed first plan: %v", err)
		}
		h := &Handler{clientStore: stores.clientStore, clientQuotaStore: stores.clientQuotaStore}
		h.router = h.routes()
		if err := stores.clientQuotaStore.Replace("source", []config.QuotaGroup{{Name: "shared"}}); err != nil {
			t.Fatal(err)
		}
		response := requestQuotaSync(h, "source", map[string]any{"clients": []string{"first", "zz-missing", "second"}})
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "not found") {
			t.Fatalf("expected unknown target error, got %d: %s", response.Code, response.Body.String())
		}
		// all-or-nothing：first 的既有 plan 未被覆盖，second 未被写入。
		firstPlan, err := stores.clientQuotaStore.Get("first")
		if err != nil {
			t.Fatalf("get first plan: %v", err)
		}
		if len(firstPlan) != 1 || firstPlan[0].PlanCoefficient() != 2 {
			t.Fatalf("first target was partially updated: %+v", firstPlan)
		}
		secondPlan, err := stores.clientQuotaStore.Get("second")
		if err != nil {
			t.Fatalf("get second plan: %v", err)
		}
		if len(secondPlan) != 0 {
			t.Fatalf("second target changed: %+v", secondPlan)
		}
	})

	t.Run("success changes only plan fields", func(t *testing.T) {
		stores := setupTestStores(t, t.TempDir())
		target := config.Client{
			Name:           "target",
			AccessKey:      "target-secret",
			IPAddress:      "192.0.2.20",
			AllowedTargets: []string{"private-a", "private-b"},
			JobLevel:       "L7",
			ProjectGroup:   "platform",
		}
		seedClients(t, stores.clientStore, []config.Client{
			{Name: "source", AccessKey: "source-key", IPAddress: "192.0.2.1"},
			target,
		})
		h := &Handler{clientStore: stores.clientStore, clientQuotaStore: stores.clientQuotaStore}
		groups := []config.QuotaGroup{{Name: "shared", Coefficient: coefPtrAdmin(0.8)}}
		if err := stores.modelGroupStore.Create(config.ModelGroup{Name: "shared", CostCoefficient: 1.5}); err != nil {
			t.Fatalf("seed model group: %v", err)
		}

		h.router = h.routes()
		if err := stores.clientQuotaStore.Replace("source", groups); err != nil {
			t.Fatal(err)
		}
		if response := requestQuotaSync(h, "source", map[string]any{"clients": []string{"target"}}); response.Code != http.StatusOK {
			t.Fatalf("sync quota: %d: %s", response.Code, response.Body.String())
		}
		// 身份/密钥/授权/组织字段与职级绑定一律不动（职级是身份属性，不随同步覆盖）。
		got := clientByName(t, stores.clientStore, "target")
		if got.Name != target.Name || got.AccessKey != target.AccessKey || got.IPAddress != target.IPAddress || !reflect.DeepEqual(got.AllowedTargets, target.AllowedTargets) || got.JobLevel != target.JobLevel || got.ProjectGroup != target.ProjectGroup {
			t.Fatalf("identity, sensitive, or organization fields changed: got %+v want %+v", got, target)
		}
		storedGroups, err := stores.clientQuotaStore.Get("target")
		if err != nil {
			t.Fatalf("get copied groups: %v", err)
		}
		if len(storedGroups) != 1 || storedGroups[0].Name != "shared" || storedGroups[0].PlanCoefficient() != 0.8 {
			t.Fatalf("plan not copied to store: %+v", storedGroups)
		}
	})

	t.Run("duplicate target aliases are deduplicated", func(t *testing.T) {
		stores := setupTestStores(t, t.TempDir())
		seedClients(t, stores.clientStore, []config.Client{
			{Name: "source", AccessKey: "source-key", IPAddress: "192.0.2.1"},
			{Name: "Target", AccessKey: "target-key", IPAddress: "192.0.2.2"},
		})
		h := &Handler{clientStore: stores.clientStore, clientQuotaStore: stores.clientQuotaStore}

		h.router = h.routes()
		response := requestQuotaSync(h, "source", map[string]any{"clients": []string{"Target", "target"}})
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"synced_clients":["Target"]`) {
			t.Fatalf("expected one canonical target, got %d: %s", response.Code, response.Body.String())
		}
		if got, err := stores.clientQuotaStore.Get("Target"); err != nil || len(got) != 0 {
			t.Fatalf("duplicate target request changed plan: %+v (err %v)", got, err)
		}
	})

	t.Run("source self sync is rejected", func(t *testing.T) {
		stores := setupTestStores(t, t.TempDir())
		seedClients(t, stores.clientStore, []config.Client{{Name: "Source", AccessKey: "source-key", IPAddress: "192.0.2.1"}})
		h := &Handler{clientStore: stores.clientStore, clientQuotaStore: stores.clientQuotaStore}

		h.router = h.routes()
		response := requestQuotaSync(h, "Source", map[string]any{"clients": []string{"source"}})
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "no sync targets") {
			t.Fatalf("expected no targets after source exclusion, got %d: %s", response.Code, response.Body.String())
		}
	})
}

// TestHandlerUpdateQuota 验证 PUT /admin/data/quota/{client} 只更新额度字段，
// 不触碰身份、密钥与授权字段。
func TestHandlerUpdateQuota(t *testing.T) {
	newHandler := func(t *testing.T) (*Handler, *nosql.ClientStore, *nosql.AuditStore, *nosql.ModelGroupStore, *nosql.ClientQuotaStore, *nosql.JobLevelStore) {
		t.Helper()
		tempDir := t.TempDir()
		db, err := nosql.OpenDB(filepath.Join(tempDir, "quota-test.db"))
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		cStore := nosql.NewClientStore(db)
		aStore := nosql.NewAuditStore(db)
		gStore := nosql.NewModelGroupStore(db)
		qStore := nosql.NewClientQuotaStore(db)
		jStore := nosql.NewJobLevelStore(db)
		return &Handler{clientStore: cStore, modelGroupStore: gStore, clientQuotaStore: qStore, jobLevelStore: jStore, auditStore: aStore}, cStore, aStore, gStore, qStore, jStore
	}

	doUpdate := func(h *Handler, client, body string) *httptest.ResponseRecorder {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("client", client)
		req := httptest.NewRequest(http.MethodPut, "http://example.com/data/quota/"+client, strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.handleUpdateQuota(rec, req)
		return rec
	}

	clientByName := func(t *testing.T, store *nosql.ClientStore, name string) config.Client {
		t.Helper()
		clients, err := store.List()
		if err != nil {
			t.Fatalf("list clients: %v", err)
		}
		for _, c := range clients {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("client %q not found", name)
		return config.Client{}
	}

	t.Run("success updates job level and plan only", func(t *testing.T) {
		h, cStore, aStore, gStore, qStore, jStore := newHandler(t)
		if err := gStore.Create(config.ModelGroup{Name: "shared", CostCoefficient: 1.5, DailyUSD: 4}); err != nil {
			t.Fatalf("create model group: %v", err)
		}
		if err := jStore.Create(config.JobLevel{Name: "L7", CoefficientDaily: coefPtrAdmin(2)}); err != nil {
			t.Fatalf("create job level: %v", err)
		}
		original := config.Client{
			Name:           "alice",
			AccessKey:      "alice-secret",
			IPAddress:      "192.0.2.10",
			AllowedTargets: []string{"private-a"},
			JobLevel:       "L6",
			ProjectGroup:   "platform",
		}
		seedClients(t, cStore, []config.Client{original})

		body := `{"job_level":"L7","quota_groups":[{"name":"shared","coefficient":0.5}]}`
		rec := doUpdate(h, "alice", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		got := clientByName(t, cStore, "alice")
		if got.Name != original.Name || got.AccessKey != original.AccessKey || got.IPAddress != original.IPAddress ||
			!reflect.DeepEqual(got.AllowedTargets, original.AllowedTargets) || got.ProjectGroup != original.ProjectGroup {
			t.Fatalf("identity or sensitive fields changed: got %+v want %+v", got, original)
		}
		if got.JobLevel != "L7" {
			t.Fatalf("job level not updated: %+v", got)
		}
		groups, err := qStore.Get("alice")
		if err != nil {
			t.Fatalf("get client quota groups: %v", err)
		}
		if len(groups) != 1 || groups[0].Name != "shared" || groups[0].PlanCoefficient() != 0.5 {
			t.Fatalf("plan not stored in quota store: %+v", groups)
		}
		events, err := aStore.List(10)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		found := false
		for _, event := range events {
			if event.Action == "quota_update" && event.Object == "alice" && event.Result == "success" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected quota_update audit event, got %+v", events)
		}
	})

	t.Run("unknown model group rejected", func(t *testing.T) {
		h, cStore, _, _, qStore, _ := newHandler(t)
		seedClients(t, cStore, []config.Client{{Name: "alice", AccessKey: "alice-secret", IPAddress: "192.0.2.10", JobLevel: "L6"}})
		rec := doUpdate(h, "alice", `{"job_level":"L6","quota_groups":[{"name":"ghost-group"}]}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not found") {
			t.Fatalf("expected 400 unknown model group, got %d: %s", rec.Code, rec.Body.String())
		}
		if got := clientByName(t, cStore, "alice"); got.JobLevel != "L6" {
			t.Fatalf("client changed after rejected update: %+v", got)
		}
		groups, err := qStore.Get("alice")
		if err != nil {
			t.Fatalf("get client quota groups: %v", err)
		}
		if len(groups) != 0 {
			t.Fatalf("quota store changed after rejected update: %+v", groups)
		}
	})

	t.Run("negative coefficient rejected", func(t *testing.T) {
		h, cStore, _, gStore, _, _ := newHandler(t)
		if err := gStore.Create(config.ModelGroup{Name: "shared", CostCoefficient: 1}); err != nil {
			t.Fatalf("create model group: %v", err)
		}
		seedClients(t, cStore, []config.Client{{Name: "alice", AccessKey: "alice-secret", IPAddress: "192.0.2.10"}})
		rec := doUpdate(h, "alice", `{"quota_groups":[{"name":"shared","coefficient":-1}]}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "negative") {
			t.Fatalf("expected 400 negative coefficient, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown job level rejected", func(t *testing.T) {
		h, cStore, _, _, qStore, _ := newHandler(t)
		seedClients(t, cStore, []config.Client{{Name: "alice", AccessKey: "alice-secret", IPAddress: "192.0.2.10"}})
		rec := doUpdate(h, "alice", `{"job_level":"ghost"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not found") {
			t.Fatalf("expected 400 unknown job level, got %d: %s", rec.Code, rec.Body.String())
		}
		if got, err := qStore.Get("alice"); err != nil || len(got) != 0 {
			t.Fatalf("rejected update must not write plan: %+v (err %v)", got, err)
		}
	})

	t.Run("unknown client returns 404", func(t *testing.T) {
		h, _, _, _, _, _ := newHandler(t)
		rec := doUpdate(h, "ghost", `{"job_level":""}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHandlerModelGroupZeroCoefficient(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	h := &Handler{modelGroupStore: stores.modelGroupStore, auditStore: stores.auditStore}

	create := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleCreateModelGroup(rec, httptest.NewRequest(http.MethodPost, "/data/model-groups", strings.NewReader(body)))
		return rec
	}
	if rec := create(`{"name":"unlimited","cost_coefficient":0,"daily_usd":10}`); rec.Code != http.StatusCreated {
		t.Fatalf("zero coefficient create: got %d: %s", rec.Code, rec.Body.String())
	}
	groups, err := stores.modelGroupStore.List()
	if err != nil || len(groups) != 1 || groups[0].CostCoefficient != 0 {
		t.Fatalf("zero coefficient must survive API storage and readback: groups=%+v err=%v", groups, err)
	}
	if rec := create(`{"name":"negative","cost_coefficient":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative coefficient create: got %d: %s", rec.Code, rec.Body.String())
	}
	if groups, err = stores.modelGroupStore.List(); err != nil || len(groups) != 1 {
		t.Fatalf("rejected create changed model groups: groups=%+v err=%v", groups, err)
	}

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "unlimited")
	req := httptest.NewRequest(http.MethodPut, "/data/model-groups/unlimited", strings.NewReader(`{"name":"unlimited","cost_coefficient":-1}`))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.handleUpdateModelGroup(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative coefficient update: got %d: %s", rec.Code, rec.Body.String())
	}
	if groups, err = stores.modelGroupStore.List(); err != nil || len(groups) != 1 || groups[0].CostCoefficient != 0 {
		t.Fatalf("rejected update changed coefficient: groups=%+v err=%v", groups, err)
	}
}

// TestHandlerQuotaStatus_ClientExceeded 验证 client 超限时返回 200 + Exceeded 非空。
// 使用独立的 bbolt DB 以便直接写入 usage_agg_hourly 数据。
func TestHandlerQuotaStatus_ClientExceeded(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	// 自建 db + stores（不依赖 setupTestStores，方便暴露 db 给 BackfillUsageAgg）
	dbPath := filepath.Join(tempDir, "quota-test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cStore := nosql.NewClientStore(db)
	mCostStore := nosql.NewModelCostStore(db)
	uStore := nosql.NewUsageStore(db)
	aStore := nosql.NewAuditStore(db)
	usStore := nosql.NewUserStore(db)
	tsStore := nosql.NewTargetStore(db)

	// 额度 v2：未绑职级的 client = 未配置额度 = 客户端级拒绝
	if err := cStore.Create(config.Client{
		Name:      "alice",
		AccessKey: "k-alice",
		IPAddress: "192.0.2.1",
	}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	// 创建模型费用
	if err := mCostStore.Upsert(nosql.ModelCost{
		Model:             "gpt-4",
		InputPer1MTokens:  30.0,
		OutputPer1MTokens: 60.0,
	}); err != nil {
		t.Fatalf("upsert cost: %v", err)
	}
	// 写入 usage event
	if err := uStore.Record(usage.Event{
		Timestamp:    time.Now().UTC(),
		ClientName:   "alice",
		EndpointType: "openai",
		Model:        "gpt-4",
		InputTokens:  200_000,
		StatusCode:   200,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := nosql.BackfillUsageAgg(db); err != nil {
		t.Fatalf("BackfillUsageAgg: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cat, _ := catalog.New()
	qm, err := quota.New(quota.Options{
		Catalog:     cat,
		ClientStore: cStore,
		UsageStore:  uStore,
		CostStore:   mCostStore,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}
	qm.Evaluate("alice")

	info, exceeded := qm.Check("alice")
	if !exceeded {
		t.Fatalf("setup: alice not exceeded after Evaluate; info=%+v", info)
	}

	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, aStore, usStore, cStore, tsStore, mCostStore, uStore, nil, qm, nil, nil, logger)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/quota/alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d. body=%s", rec.Code, rec.Body.String())
	}

	var status quota.QuotaStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v. body=%s", err, rec.Body.String())
	}
	if status.Client != "alice" {
		t.Errorf("expected client 'alice', got %q", status.Client)
	}
	if status.Exceeded == nil {
		t.Fatal("expected Exceeded to be non-nil for client without job level")
	}
	if status.Exceeded.Scope != quota.ScopeJobLevel || status.Exceeded.Dimension != quota.JobLevelUnassignedDimension {
		t.Errorf("expected job_level/unassigned exceeded, got %+v", *status.Exceeded)
	}
	if !status.JobLevelMissing {
		t.Error("expected job_level_missing flag in status")
	}
}

func TestReadonlyGuardMiddleware(t *testing.T) {
	cfg := config.AdminSessionConfig{
		Secret:     "test-guard-secret",
		CookieName: "test-guard-session",
		TTLSeconds: 3600,
	}
	sm := NewSessionManager(cfg, nil)
	_, adminCookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatalf("failed to create admin session: %v", err)
	}
	_, readonlyCookie, err := sm.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("failed to create readonly session: %v", err)
	}

	// Create a router that mimics the admin handler's guard setup.
	guard := sm.ReadonlyGuard()
	router := chi.NewRouter()
	router.Use(sm.Middleware) // session auth first

	// Read route (no guard)
	router.Get("/data/targets", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"targets":[]}`))
	})

	// Write route (with guard)
	router.With(guard).Post("/data/targets", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	router.With(guard).Put("/data/targets/x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.With(guard).Delete("/data/targets/x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name   string
		method string
		path   string
		cookie *http.Cookie
		want   int
	}{
		// Admin can read and write.
		{"admin GET targets", http.MethodGet, "/data/targets", adminCookie, http.StatusOK},
		{"admin POST targets", http.MethodPost, "/data/targets", adminCookie, http.StatusCreated},
		{"admin PUT targets", http.MethodPut, "/data/targets/x", adminCookie, http.StatusOK},
		{"admin DELETE targets", http.MethodDelete, "/data/targets/x", adminCookie, http.StatusOK},
		// Readonly can read but not write.
		{"readonly GET targets", http.MethodGet, "/data/targets", readonlyCookie, http.StatusOK},
		{"readonly POST targets blocked", http.MethodPost, "/data/targets", readonlyCookie, http.StatusForbidden},
		{"readonly PUT targets blocked", http.MethodPut, "/data/targets/x", readonlyCookie, http.StatusForbidden},
		{"readonly DELETE targets blocked", http.MethodDelete, "/data/targets/x", readonlyCookie, http.StatusForbidden},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://example.com"+tt.path, nil)
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("expected %d, got %d; body: %s", tt.want, rec.Code, rec.Body.String())
			}
			if tt.want == http.StatusForbidden {
				var resp map[string]string
				if err := json.NewDecoder(rec.Body).Decode(&resp); err == nil {
					if !strings.Contains(resp["error"], "readonly") {
						t.Errorf("expected error message to mention 'readonly', got %q", resp["error"])
					}
				}
			}
		})
	}
}

func TestUsageModelGroupReportReadonlyAccess(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 0, nil)
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sessionManager := NewSessionManager(config.AdminSessionConfig{
		Secret: "usage-model-group-report-secret", CookieName: "usage-model-group-report-session", TTLSeconds: 3600,
	}, logger)
	clientAuth := auth.NewStore()
	manager := config.NewManager(configPath)
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}
	h := NewHandler(manager, clientAuth, service, stores.auditStore, stores.userStore, stores.clientStore,
		stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessionManager, logger)
	router := chi.NewRouter()
	router.Use(sessionManager.Middleware)
	router.Mount("/", h)
	_, readonlyCookie, err := sessionManager.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("readonly session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/model-groups", nil)
	req.AddCookie(readonlyCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readonly GET /data/usage/model-groups: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadonlyGuardAllowsChangePassword(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	// Create a readonly user with password.
	readonlyPwHash, _ := HashPasswordWithRandomSalt("readonly-pass")
	if err := stores.userStore.Create(config.AdminUser{
		Username:     "auditor",
		PasswordHash: readonlyPwHash,
		Role:         RoleReadonly,
	}); err != nil {
		t.Fatalf("failed to create readonly user: %v", err)
	}

	// Setup session manager and handler.
	smCfg := config.AdminSessionConfig{
		Secret:     "test-guard-secret",
		CookieName: "test-guard-session",
		TTLSeconds: 3600,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(smCfg, logger)

	manager := config.NewManager(configPath)
	store := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("failed to init catalog: %v", err)
	}
	quotaManager, err := quota.New(quota.Options{
		Catalog:     cat,
		ClientStore: stores.clientStore,
		UsageStore:  stores.usageStore,
		CostStore:   stores.modelCostStore,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("failed to init quota manager: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, cat, quotaManager, nil, sm, logger)

	// Wrap with session middleware.
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)

	// Create a readonly session.
	_, roCookie, err := sm.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("failed to create readonly session: %v", err)
	}

	// Test: readonly user can GET /data/targets (read).
	t.Run("readonly can GET targets", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil)
		req.AddCookie(roCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	for _, path := range []string{"/data/quota/summary", "/data/quota/alice", "/data/model-groups", "/data/job-levels", "/data/project-groups"} {
		path := path
		t.Run("readonly can GET "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
			req.AddCookie(roCookie)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("readonly GET %s expected 200, got %d: %s", path, rec.Code, rec.Body.String())
			}
		})
	}

	quotaWrites := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/data/quota/alice/sync"},
		{http.MethodPut, "/data/quota/alice"},
		{http.MethodDelete, "/data/quota/alice"},
		{http.MethodPost, "/data/model-groups"},
		{http.MethodPut, "/data/model-groups/default"},
		{http.MethodDelete, "/data/model-groups/default"},
		{http.MethodPost, "/data/job-levels"},
		{http.MethodPut, "/data/job-levels/L6"},
		{http.MethodDelete, "/data/job-levels/L6"},
		{http.MethodPost, "/data/project-groups"},
		{http.MethodPut, "/data/project-groups/platform"},
		{http.MethodDelete, "/data/project-groups/platform"},
		{http.MethodPut, "/data/clients/alice"},
	}
	for _, tt := range quotaWrites {
		tt := tt
		t.Run("readonly blocked from "+tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://example.com"+tt.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(roCookie)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("readonly %s %s expected 403, got %d: %s", tt.method, tt.path, rec.Code, rec.Body.String())
			}
		})
	}

	// The readonly checks above must happen before handlers can write audit
	// events; successful model-group mutations still retain their audit trail.
	_, adminCookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatalf("failed to create admin session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/data/model-groups", strings.NewReader(`{"name":"audit-group","cost_coefficient":1,"bindings":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin model-group create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	events, err := stores.auditStore.List(20)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Action == "model_group_create" && event.Object == "audit-group" && event.Result == "success" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("model-group create audit event missing: %+v", events)
	}

	// Test: readonly user is blocked from POST /data/targets (write).
	t.Run("readonly blocked from POST targets", func(t *testing.T) {
		body := []byte(`{"name":"test","endpoint_type":"openai","endpoint":"https://api.openai.com","api_key":"sk-test","usage_adapter":"standard"}`)
		req := httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(roCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	// Test: readonly user can POST /api/change-password (special exception).
	t.Run("readonly can change own password", func(t *testing.T) {
		body := []byte(`{"old_password":"readonly-pass","new_password":"new-pass-123"}`)
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/change-password", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(roCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	// Test: readonly user is blocked from POST /config/reload.
	t.Run("readonly blocked from config reload", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
		req.AddCookie(roCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	// Test: readonly user is blocked from user management (admin-only).
	t.Run("readonly blocked from list users", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/data/users", nil)
		req.AddCookie(roCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestUserEndpointsCRUD(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	// Seed the default admin account plus a second admin for self-role checks.
	defaultHash, err := HashPasswordWithRandomSalt("default-admin-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := stores.userStore.Create(config.AdminUser{Username: DefaultAdminUsername, PasswordHash: defaultHash, Role: RoleAdmin}); err != nil {
		t.Fatalf("seed default admin: %v", err)
	}

	smCfg := config.AdminSessionConfig{
		Secret: "test-users-secret", CookieName: "test-users-session", TTLSeconds: 3600,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(smCfg, logger)
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("failed to init catalog: %v", err)
	}
	quotaManager, err := quota.New(quota.Options{
		Catalog:     cat,
		ClientStore: stores.clientStore,
		UsageStore:  stores.usageStore,
		CostStore:   stores.modelCostStore,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("failed to init quota manager: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, cat, quotaManager, nil, sm, logger)
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)

	_, adminCookie, err := sm.CreateSession(DefaultAdminUsername, RoleAdmin)
	if err != nil {
		t.Fatalf("admin session: %v", err)
	}

	do := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, "http://example.com"+path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("admin can create user", func(t *testing.T) {
		rec := do(http.MethodPost, "/data/users", `{"username":"op-user","password":"op-pass-123","role":"readonly"}`, adminCookie)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin can update role and password", func(t *testing.T) {
		rec := do(http.MethodPut, "/data/users/op-user", `{"role":"admin","password":"new-pass-456"}`, adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		u, err := stores.userStore.Get("op-user")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if u.Role != RoleAdmin {
			t.Errorf("role not updated: %+v", u)
		}
		ok, _ := verifyPasswordHash(u.PasswordHash, "new-pass-456")
		if !ok {
			t.Errorf("password not updated")
		}
	})

	t.Run("admin can disable and re-enable user", func(t *testing.T) {
		rec := do(http.MethodPut, "/data/users/op-user", `{"disabled":true}`, adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		rec = do(http.MethodPut, "/data/users/op-user", `{"disabled":false}`, adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("default admin account protected from update", func(t *testing.T) {
		rec := do(http.MethodPut, "/data/users/"+DefaultAdminUsername, `{"role":"readonly"}`, adminCookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("default admin account protected from delete", func(t *testing.T) {
		rec := do(http.MethodDelete, "/data/users/"+DefaultAdminUsername, "", adminCookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("second admin self role change rejected", func(t *testing.T) {
		_, opCookie, err := sm.CreateSession("op-user", RoleAdmin)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		rec := do(http.MethodPut, "/data/users/op-user", `{"role":"readonly"}`, opCookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin can delete user", func(t *testing.T) {
		rec := do(http.MethodDelete, "/data/users/op-user", "", adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if _, err := stores.userStore.Get("op-user"); err == nil {
			t.Errorf("user should be deleted")
		}
	})
}

func TestDictEndpointsCRUD(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(config.AdminSessionConfig{
		Secret: "test-dict-secret", CookieName: "test-dict-session", TTLSeconds: 3600,
	}, logger)
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)

	_, adminCookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatalf("failed to create admin session: %v", err)
	}
	doJSON := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := doJSON(http.MethodPost, "/data/job-levels", `{"name":"L6"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create job level expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodPost, "/data/job-levels", `{"name":"l6"}`); rec.Code != http.StatusConflict {
		t.Fatalf("case-insensitive duplicate expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodPost, "/data/project-groups", `{"name":"platform"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create project group expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := doJSON(http.MethodPost, "/data/job-levels", `{"name":"L5","coefficient_daily":0,"coefficient_weekly":1.5}`); rec.Code != http.StatusCreated {
		t.Fatalf("create structured job level expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodPost, "/data/job-levels", `{"name":"L4","coefficient_daily":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative coefficient expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodGet, "/data/job-levels", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"L6"`) || !strings.Contains(rec.Body.String(), `"coefficient_weekly":1.5`) {
		t.Fatalf("list job levels unexpected: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodGet, "/data/project-groups", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"project_groups":["platform"]`) {
		t.Fatalf("list project groups unexpected: %d %s", rec.Code, rec.Body.String())
	}

	// 重命名级联：绑定 L6 的客户端同步改为 L7。
	if err := stores.clientStore.Create(config.Client{Name: "carol", AccessKey: "carol-key", IPAddress: "192.0.2.50", JobLevel: "L6"}); err != nil {
		t.Fatalf("seed carol: %v", err)
	}
	if rec := doJSON(http.MethodPut, "/data/job-levels/L6", `{"name":"L7"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename job level expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodGet, "/data/job-levels", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"L7"`) {
		t.Fatalf("list job levels after rename unexpected: %d %s", rec.Code, rec.Body.String())
	}
	if carol, err := stores.clientStore.Get("carol"); err != nil || carol == nil || carol.JobLevel != "L7" {
		t.Fatalf("rename did not cascade to client: %+v (err %v)", carol, err)
	}
	// 删除仍被引用的职级 → 409 且列出客户端名。
	if rec := doJSON(http.MethodDelete, "/data/job-levels/L7", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "carol") {
		t.Fatalf("delete referenced job level expected 409 with client name, got %d: %s", rec.Code, rec.Body.String())
	}
	// 改绑后删除成功（204）。
	if err := stores.clientStore.Update("carol", config.Client{Name: "carol", AccessKey: "carol-key", IPAddress: "192.0.2.50", JobLevel: "L5"}); err != nil {
		t.Fatalf("rebind carol: %v", err)
	}
	if rec := doJSON(http.MethodDelete, "/data/job-levels/L7", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete unreferenced job level expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := doJSON(http.MethodDelete, "/data/project-groups/platform", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete project group expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(http.MethodGet, "/data/project-groups", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("list project groups after delete unexpected: %d %s", rec.Code, rec.Body.String())
	}

	events, err := stores.auditStore.List(20)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	for _, want := range []struct{ action, object string }{
		{"job_level_create", "L6"},
		{"project_group_create", "platform"},
		{"job_level_update", "L6"},
		{"project_group_delete", "platform"},
	} {
		found := false
		for _, event := range events {
			if event.Action == want.action && event.Object == want.object && event.Result == "success" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("audit event missing: %s %s; got %+v", want.action, want.object, events)
		}
	}
}

func TestPricingTemplatesCRUD(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(config.AdminSessionConfig{
		Secret: "test-pricing-secret", CookieName: "test-pricing-session", TTLSeconds: 3600,
	}, logger)
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)

	_, adminCookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatalf("failed to create admin session: %v", err)
	}
	doJSON := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	validBody := `{"target":"","model":"gpt-4o","mode":"flat","base_rates":{"input_per_1m_tokens":10,"output_per_1m_tokens":20,"cache_write_per_1m_tokens":3,"cache_read_per_1m_tokens":0.3}}`

	// 1. Create → 201 with non-empty id.
	rec := doJSON(http.MethodPost, "/data/pricing-templates", validBody, adminCookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pricing template expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK       bool                    `json:"ok"`
		Template pricing.PricingTemplate `json:"template"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !created.OK || created.Template.ID == "" {
		t.Fatalf("expected non-empty template id, got %+v", created)
	}
	id := created.Template.ID

	// 2. List → count >= 1 and the created template is present.
	listAndFind := func(wantID string) pricing.PricingTemplate {
		t.Helper()
		rec := doJSON(http.MethodGet, "/data/pricing-templates", "", adminCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("list pricing templates expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var list struct {
			Templates []pricing.PricingTemplate `json:"templates"`
			Count     int                       `json:"count"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		if list.Count < 1 {
			t.Fatalf("expected count >= 1, got %d", list.Count)
		}
		for _, tmpl := range list.Templates {
			if tmpl.ID == wantID {
				return tmpl
			}
		}
		t.Fatalf("created template %q not present in list: %s", wantID, rec.Body.String())
		return pricing.PricingTemplate{}
	}
	got := listAndFind(id)
	if got.BaseRates.InputPer1MTokens != 10 || got.BaseRates.OutputPer1MTokens != 20 {
		t.Fatalf("unexpected created rates: %+v", got.BaseRates)
	}

	// 3. Update → 200 and updated rates visible on follow-up GET.
	updateBody := `{"target":"","model":"gpt-4o","mode":"flat","base_rates":{"input_per_1m_tokens":99,"output_per_1m_tokens":88,"cache_write_per_1m_tokens":7,"cache_read_per_1m_tokens":0.7}}`
	rec = doJSON(http.MethodPut, "/data/pricing-templates/"+id, updateBody, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("update pricing template expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got = listAndFind(id)
	if got.BaseRates.InputPer1MTokens != 99 || got.BaseRates.OutputPer1MTokens != 88 {
		t.Fatalf("unexpected updated rates: %+v", got.BaseRates)
	}

	// 4. Delete → 204; follow-up GET shows it gone.
	rec = doJSON(http.MethodDelete, "/data/pricing-templates/"+id, "", adminCookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete pricing template expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(http.MethodGet, "/data/pricing-templates", "", adminCookie)
	var list struct {
		Templates []pricing.PricingTemplate `json:"templates"`
		Count     int                       `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode list after delete: %v", err)
	}
	for _, tmpl := range list.Templates {
		if tmpl.ID == id {
			t.Fatalf("deleted template %q still present: %s", id, rec.Body.String())
		}
	}

	// 5. Invalid body (negative rate) → 400.
	invalidBody := `{"target":"","model":"gpt-4o","mode":"flat","base_rates":{"input_per_1m_tokens":-1,"output_per_1m_tokens":20,"cache_write_per_1m_tokens":3,"cache_read_per_1m_tokens":0.3}}`
	rec = doJSON(http.MethodPost, "/data/pricing-templates", invalidBody, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid create expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Readonly session is blocked from the write route.
	_, readonlyCookie, err := sm.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("failed to create readonly session: %v", err)
	}
	rec = doJSON(http.MethodPost, "/data/pricing-templates", validBody, readonlyCookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly POST pricing-templates expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPricingTemplateUpdateRejectsKeyDrift(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(config.AdminSessionConfig{
		Secret: "test-drift-secret", CookieName: "test-drift-session", TTLSeconds: 3600,
	}, logger)
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)

	_, adminCookie, err := sm.CreateSession("admin", RoleAdmin)
	if err != nil {
		t.Fatalf("failed to create admin session: %v", err)
	}
	doJSON := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	createBody := `{"target":"t1","model":"gpt-4o","mode":"flat","base_rates":{"input_per_1m_tokens":10,"output_per_1m_tokens":20,"cache_write_per_1m_tokens":3,"cache_read_per_1m_tokens":0.3}}`
	rec := doJSON(http.MethodPost, "/data/pricing-templates", createBody, adminCookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pricing template expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Template pricing.PricingTemplate `json:"template"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id := created.Template.ID

	// Drifted model on update → 400.
	driftBody := `{"target":"t1","model":"gpt-4o-mini","mode":"flat","base_rates":{"input_per_1m_tokens":10,"output_per_1m_tokens":20,"cache_write_per_1m_tokens":3,"cache_read_per_1m_tokens":0.3}}`
	rec = doJSON(http.MethodPut, "/data/pricing-templates/"+id, driftBody, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with drifted model expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Drifted target on update → 400.
	driftBody = `{"target":"t2","model":"gpt-4o","mode":"flat","base_rates":{"input_per_1m_tokens":10,"output_per_1m_tokens":20,"cache_write_per_1m_tokens":3,"cache_read_per_1m_tokens":0.3}}`
	rec = doJSON(http.MethodPut, "/data/pricing-templates/"+id, driftBody, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with drifted target expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown id → 404.
	rec = doJSON(http.MethodPut, "/data/pricing-templates/no-such-id", createBody, adminCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update unknown id expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Same target/model with different case/whitespace is NOT drift → 200.
	sameKeyBody := `{"target":" T1 ","model":"GPT-4O","mode":"flat","base_rates":{"input_per_1m_tokens":11,"output_per_1m_tokens":21,"cache_write_per_1m_tokens":3,"cache_read_per_1m_tokens":0.3}}`
	rec = doJSON(http.MethodPut, "/data/pricing-templates/"+id, sameKeyBody, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("update with same key (case-insensitive) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// List must contain exactly one entry for the id (no ghost duplicates).
	rec = doJSON(http.MethodGet, "/data/pricing-templates", "", adminCookie)
	var list struct {
		Templates []pricing.PricingTemplate `json:"templates"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	count := 0
	for _, tmpl := range list.Templates {
		if tmpl.ID == id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 template with id %q after updates, got %d", id, count)
	}
}

func TestGetResolverAppliesModelGroupCoefficient(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	if err := stores.targetStore.Create(config.Target{Name: "t1", EndpointType: "openai", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "m1"}}}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := stores.modelGroupStore.Create(config.ModelGroup{Name: "discounted", CostCoefficient: 0.5, Bindings: []config.QuotaGroupBinding{{Target: "t1"}}}); err != nil {
		t.Fatalf("create model group: %v", err)
	}
	if err := stores.modelCostStore.Upsert(nosql.ModelCost{Model: "m1", InputPer1MTokens: 10, OutputPer1MTokens: 20, CacheWritePer1MToken: 3, CacheReadPer1MToken: 0.5}); err != nil {
		t.Fatalf("seed model cost: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(config.AdminSessionConfig{
		Secret: "test-coef-secret", CookieName: "test-coef-session", TTLSeconds: 3600,
	}, logger)
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	_ = NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)
	// getResolver only depends on costCache/modelCostStore/modelCatalog/modelGroupStore;
	// construct the struct directly to exercise the handler wiring without a router.
	hh := &Handler{
		modelCostStore:  stores.modelCostStore,
		modelGroupStore: stores.modelGroupStore,
		sessionManager:  sm,
		logger:          logger,
	}

	resolver := hh.getResolver()
	if resolver == nil {
		t.Fatal("expected resolver")
	}
	rates, ok := resolver.RateFor("t1", "m1", time.Now().UTC())
	if !ok {
		t.Fatal("expected rate resolution")
	}
	if rates.InputPer1MTokens != 5 || rates.OutputPer1MTokens != 10 || rates.CacheWritePer1MToken != 1.5 || rates.CacheReadPer1MToken != 0.25 {
		t.Fatalf("expected group-discounted rates (x0.5), got %+v", rates)
	}

	// Unmatched target keeps raw rates.
	rates, ok = resolver.RateFor("other-target", "m1", time.Now().UTC())
	if !ok || rates.InputPer1MTokens != 10 {
		t.Fatalf("expected raw rates for unmatched target, got %+v (ok=%v)", rates, ok)
	}
}

// ---------- Deployment pool tests (ported from main, S1-S5) ----------

// newPoolTestHandler builds a handler with one seeded target and returns the
// auth store so tests can inspect principal expansion after admin writes.
func newPoolTestHandler(t *testing.T, tempDir string) (http.Handler, *auth.Store, testStores, *proxy.Service) {
	t.Helper()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.Targets[0].APIKey = "sk-primary-real-0001"
	cfg.Targets[0].APIKeys = []string{"sk-secondary-real-0002"}
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	return h, authStore, stores, service
}

func mustGetPool(t *testing.T, store *nosql.PoolStore, name string) config.DeploymentPool {
	t.Helper()
	pools, err := store.List()
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	for _, p := range pools {
		if strings.EqualFold(p.Name, name) {
			return p
		}
	}
	t.Fatalf("pool %q not found", name)
	return config.DeploymentPool{}
}

func poolNames(store *nosql.PoolStore) ([]string, error) {
	pools, err := store.List()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		names = append(names, p.Name)
	}
	return names, nil
}

// poolTargetUpdateBody wraps targetUpdateBody with a pool assignment.
func poolTargetUpdateBody(pool string) map[string]any {
	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"})
	body["pool"] = pool
	return body
}

func postPool(t *testing.T, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal pool body: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/pools", bytes.NewReader(payload)))
	return rec
}

func putPool(t *testing.T, h http.Handler, name string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal pool body: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/pools/"+name, bytes.NewReader(payload)))
	return rec
}

// TestPoolCRUDStoreAndRuntime mirrors the double-write assertions of
// TestHandlerUpdateTargetSetsPausedInStoreAndRuntime for pools: the bbolt
// store and the proxy runtime must agree after every admin write.
func TestPoolCRUDStoreAndRuntime(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores, service := newPoolTestHandler(t, tempDir)
	poolStore := stores.targetStore.Pools()

	rec := postPool(t, h, map[string]any{"name": "alpha", "description": "primary pool"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	created := mustGetPool(t, poolStore, "alpha")
	if created.Description != "primary pool" || created.Paused {
		t.Fatalf("unexpected stored pool: %+v", created)
	}

	// Duplicate name → 409.
	rec = postPool(t, h, map[string]any{"name": "alpha"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate pool expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Empty name → 400.
	rec = postPool(t, h, map[string]any{"name": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty pool name expected 400, got %d", rec.Code)
	}

	// List reflects the store.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/pools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list pools expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Pools []config.DeploymentPool `json:"pools"`
		Count int                     `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode pools list: %v", err)
	}
	if listResp.Count != 1 || len(listResp.Pools) != 1 || listResp.Pools[0].Name != "alpha" {
		t.Fatalf("unexpected pools list: %+v", listResp)
	}

	// Assign target-1 to the pool via target update.
	rec = putTarget(t, h, "target-1", poolTargetUpdateBody("alpha"))
	if rec.Code != http.StatusOK {
		t.Fatalf("assign target to pool expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.Pool != "alpha" {
		t.Fatalf("expected target pool persisted, got %q", updated.Pool)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Paused {
		t.Fatalf("unpaused pool must not pause member at runtime, got %#v", statuses)
	}

	// Update description (name immutable).
	rec = putPool(t, h, "alpha", map[string]any{"description": "renamed desc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update pool expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if p := mustGetPool(t, poolStore, "alpha"); p.Description != "renamed desc" {
		t.Fatalf("description not persisted: %+v", p)
	}

	// Rename attempt → 400.
	rec = putPool(t, h, "alpha", map[string]any{"name": "beta"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "pool name cannot be changed") {
		t.Fatalf("rename expected 400 with message, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Unknown pool update → 404.
	rec = putPool(t, h, "ghost", map[string]any{"description": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pool update expected 404, got %d", rec.Code)
	}

	// Unassign then delete.
	rec = putTarget(t, h, "target-1", poolTargetUpdateBody(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/pools/alpha", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete pool expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	names, err := poolNames(poolStore)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected pool bucket empty after delete, got %v", names)
	}
}

// TestPoolPausePropagatesAndPreservesAPIKeys walks PUT /data/pools/{name}
// (per frozen design) and the dedicated pause/resume routes, asserting member
// runtime pause plus key preservation in the store.
func TestPoolPausePropagatesAndPreservesAPIKeys(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.Targets[0].APIKey = initialKeys[0]
	cfg.Targets[0].APIKeys = append([]string(nil), initialKeys[1:]...)
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	poolStore := stores.targetStore.Pools()

	if rec := postPool(t, h, map[string]any{"name": "beta"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	assignBody := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1", "__existing_key_index__:2"})
	assignBody["pool"] = "beta"
	rec := putTarget(t, h, "target-1", assignBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Pause via generic update endpoint (frozen contract path).
	rec = putPool(t, h, "beta", map[string]any{"paused": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("pause pool expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if p := mustGetPool(t, poolStore, "beta"); !p.Paused {
		t.Fatalf("expected paused=true in pool store, got %+v", p)
	}
	paused := mustGetTarget(t, stores.targetStore, "target-1")
	if paused.APIKey != initialKeys[0] || fmt.Sprint(paused.APIKeys) != fmt.Sprint(initialKeys[1:]) {
		t.Fatalf("keys changed on pool pause: %#v", paused)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true for pool member, got %#v", statuses)
	}

	// Resume via dedicated route.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/pools/beta/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume pool expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if p := mustGetPool(t, poolStore, "beta"); p.Paused {
		t.Fatalf("expected paused=false after resume, got %+v", p)
	}
	resumed := mustGetTarget(t, stores.targetStore, "target-1")
	if resumed.APIKey != initialKeys[0] || fmt.Sprint(resumed.APIKeys) != fmt.Sprint(initialKeys[1:]) {
		t.Fatalf("keys changed on pool resume: %#v", resumed)
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false after resume, got %#v", statuses)
	}

	// Dedicated pause route also propagates.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/pools/beta/pause", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause pool route expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true via pause route, got %#v", statuses)
	}

	// Pausing an unknown pool → 404.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/pools/ghost/pause", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pool pause expected 404, got %d", rec.Code)
	}
}

// TestPoolDeleteRejectedWithMembers asserts 409 with member names when the
// pool still has member targets (frozen delete semantics).
func TestPoolDeleteRejectedWithMembers(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores, _ := newPoolTestHandler(t, tempDir)
	poolStore := stores.targetStore.Pools()

	if rec := postPool(t, h, map[string]any{"name": "gamma"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool expected 201, got %d", rec.Code)
	}
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("gamma")); rec.Code != http.StatusOK {
		t.Fatalf("assign target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/pools/gamma", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete pool with members expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body.Error, "target-1") || !strings.Contains(body.Error, "1 members") {
		t.Fatalf("expected member list in error, got %q", body.Error)
	}
	if _, err := poolNames(poolStore); err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if p := mustGetPool(t, poolStore, "gamma"); p.Name != "gamma" {
		t.Fatalf("pool must survive rejected delete, got %+v", p)
	}
}

// TestTargetRejectsUnknownPool asserts 400 "unknown pool" on create and
// update paths when referencing a pool that does not exist.
func TestTargetRejectsUnknownPool(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores, _ := newPoolTestHandler(t, tempDir)

	createBody := map[string]any{
		"name":          "new-target",
		"usage_adapter": "standard",
		"endpoint_type": "openai",
		"endpoint":      "https://example.com",
		"api_key":       "sk-new",
		"pool":          "ghost",
	}
	payload, _ := json.Marshal(createBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/targets", bytes.NewReader(payload)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown pool") {
		t.Fatalf("create with unknown pool expected 400 unknown pool, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = putTarget(t, h, "target-1", poolTargetUpdateBody("ghost"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown pool") {
		t.Fatalf("update with unknown pool expected 400 unknown pool, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Target must be untouched by the rejected update.
	if updated := mustGetTarget(t, stores.targetStore, "target-1"); updated.Pool != "" {
		t.Fatalf("rejected update must not assign pool, got %q", updated.Pool)
	}
}

// TestTargetPoolAssignmentUpdatesAuthImmediately proves the allowed_pools
// expansion snapshot is rebuilt through applyConfigRuntime: moving a member
// in or out of a pool refreshes client permissions without restart.
// It also guards the escalation bug: a pool-only client must not be allow-all.
func TestTargetPoolAssignmentUpdatesAuthImmediately(t *testing.T) {
	tempDir := t.TempDir()
	h, authStore, stores, _ := newPoolTestHandler(t, tempDir)

	if rec := postPool(t, h, map[string]any{"name": "alpha"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Pool-only client (empty allowed_targets + one allowed_pool).
	payload, _ := json.Marshal(map[string]any{
		"name":            "poolclient",
		"access_key":      "kp",
		"allowed_targets": []string{},
		"allowed_pools":   []string{"alpha"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	principal, ok := authStore.Authenticate("kp")
	if !ok {
		t.Fatal("expected auth store to contain kp")
	}
	if principal.AllowAll() {
		t.Fatal("pool-only client must not be allow-all (privilege escalation guard)")
	}
	if principal.CanAccess("target-1") {
		t.Fatal("target-1 not yet a member; client must not access it")
	}

	// Assign → permission appears immediately.
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("alpha")); rec.Code != http.StatusOK {
		t.Fatalf("assign target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	principal, _ = authStore.Authenticate("kp")
	if !principal.CanAccess("target-1") {
		t.Fatal("expected immediate access after pool assignment")
	}
	if updated := mustGetTarget(t, stores.targetStore, "target-1"); updated.Pool != "alpha" {
		t.Fatalf("expected pool persisted on target, got %q", updated.Pool)
	}

	// Unassign → permission disappears immediately.
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("")); rec.Code != http.StatusOK {
		t.Fatalf("unassign target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	principal, _ = authStore.Authenticate("kp")
	if principal.CanAccess("target-1") {
		t.Fatal("expected access revoked after pool unassignment")
	}
}

// TestClientAllowedPoolsPrincipalExpansion asserts the frozen expansion
// semantics: union of allowed_targets and pool members; unknown pools grant
// nothing; the field round-trips through the client store and API.
func TestClientAllowedPoolsPrincipalExpansion(t *testing.T) {
	tempDir := t.TempDir()
	h, authStore, stores, _ := newPoolTestHandler(t, tempDir)

	if rec := postPool(t, h, map[string]any{"name": "alpha"}); rec.Code != http.StatusCreated {
		t.Fatalf("create pool expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("alpha")); rec.Code != http.StatusOK {
		t.Fatalf("assign target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Union client: direct target grant + pool grant.
	payload, _ := json.Marshal(map[string]any{
		"name":            "union",
		"access_key":      "ku",
		"allowed_targets": []string{"target-9"},
		"allowed_pools":   []string{"alpha"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create union client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	p, ok := authStore.Authenticate("ku")
	if !ok {
		t.Fatal("expected auth store to contain ku")
	}
	if p.AllowAll() {
		t.Fatal("union client must not be allow-all")
	}
	if !p.CanAccess("target-1") {
		t.Fatal("expected access to pool member target-1")
	}
	if !p.CanAccess("target-9") {
		t.Fatal("expected access to directly allowed target-9")
	}
	if p.CanAccess("target-8") {
		t.Fatal("must not access unrelated target-8")
	}

	// Unknown pool grants nothing and must not make the client allow-all.
	payload, _ = json.Marshal(map[string]any{
		"name":            "ghostc",
		"access_key":      "kg",
		"allowed_targets": []string{},
		"allowed_pools":   []string{"ghost"},
	})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ghost-pool client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	pg, ok := authStore.Authenticate("kg")
	if !ok {
		t.Fatal("expected auth store to contain kg")
	}
	if pg.AllowAll() {
		t.Fatal("unknown-pool client must not be allow-all")
	}
	if pg.CanAccess("target-1") {
		t.Fatal("unknown pool must grant nothing")
	}

	// allowed_pools round-trips through GET /data/clients.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/clients", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list clients expected 200, got %d", rec.Code)
	}
	var listResp struct {
		Clients []config.Client `json:"clients"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode clients: %v", err)
	}
	found := false
	for _, c := range listResp.Clients {
		if c.Name == "union" {
			found = true
			if fmt.Sprint(c.AllowedPools) != fmt.Sprint([]string{"alpha"}) {
				t.Fatalf("allowed_pools not round-tripped: %#v", c.AllowedPools)
			}
		}
	}
	if !found {
		t.Fatal("union client missing from list")
	}
	_ = stores
}

// TestReadonlyRoleRejectedOnPoolWrites mirrors the session/role guard cases:
// readonly sessions may read pools but all pool writes (including dedicated
// pause/resume routes) must be rejected with 403.
func TestReadonlyRoleRejectedOnPoolWrites(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)

	readonlyPwHash, _ := HashPasswordWithRandomSalt("readonly-pass")
	if err := stores.userStore.Create(config.AdminUser{
		Username:     "auditor",
		PasswordHash: readonlyPwHash,
		Role:         RoleReadonly,
	}); err != nil {
		t.Fatalf("failed to create readonly user: %v", err)
	}

	smCfg := config.AdminSessionConfig{
		Secret:     "test-guard-secret",
		CookieName: "test-guard-session",
		TTLSeconds: 3600,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	sm := NewSessionManager(smCfg, logger)

	manager := config.NewManager(configPath)
	authStore := auth.NewStore()
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sm, logger)

	router := chi.NewRouter()
	router.Use(sm.Middleware)
	router.Mount("/", h)

	_, roCookie, err := sm.CreateSession("auditor", RoleReadonly)
	if err != nil {
		t.Fatalf("failed to create readonly session: %v", err)
	}

	do := func(method, path string, body []byte) int {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req := httptest.NewRequest(method, "http://example.com"+path, reader)
		req.AddCookie(roCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := do(http.MethodGet, "/data/pools", nil); code != http.StatusOK {
		t.Errorf("readonly GET pools expected 200, got %d", code)
	}
	if code := do(http.MethodPost, "/data/pools", []byte(`{"name":"x"}`)); code != http.StatusForbidden {
		t.Errorf("readonly POST pools expected 403, got %d", code)
	}
	if code := do(http.MethodPut, "/data/pools/x", []byte(`{"description":"d"}`)); code != http.StatusForbidden {
		t.Errorf("readonly PUT pools expected 403, got %d", code)
	}
	if code := do(http.MethodDelete, "/data/pools/x", nil); code != http.StatusForbidden {
		t.Errorf("readonly DELETE pools expected 403, got %d", code)
	}
	if code := do(http.MethodPut, "/data/pools/x/pause", nil); code != http.StatusForbidden {
		t.Errorf("readonly PUT pools pause expected 403, got %d", code)
	}
	if code := do(http.MethodPut, "/data/pools/x/resume", nil); code != http.StatusForbidden {
		t.Errorf("readonly PUT pools resume expected 403, got %d", code)
	}
}

// TestReloadConfigOverlaysPools asserts the reload path: file pools are
// migrated into the bucket on first reload, and once the bucket is
// authoritative the runtime keeps store pools even when the file changes.
func TestReloadConfigOverlaysPools(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initial.Pools = []config.DeploymentPool{{Name: "filepool", Description: "from file"}}
	initialClients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, initial)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, initialClients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	poolStore := stores.targetStore.Pools()

	// First reload: bucket empty → file pools persisted into the bucket.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first reload expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if p := mustGetPool(t, poolStore, "filepool"); p.Description != "from file" {
		t.Fatalf("file pools not migrated into bucket: %+v", p)
	}

	// GET /data/pools serves the store-backed view.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/pools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list pools expected 200, got %d", rec.Code)
	}
	var listResp struct {
		Pools []config.DeploymentPool `json:"pools"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode pools: %v", err)
	}
	if len(listResp.Pools) != 1 || listResp.Pools[0].Name != "filepool" {
		t.Fatalf("unexpected pools after reload: %+v", listResp.Pools)
	}

	// Rewrite the file with different pools; the bucket must win.
	updated := testConfig(tempDir, 1, []string{"k1"})
	updated.Pools = []config.DeploymentPool{{Name: "otherpool"}}
	writeConfigFile(t, configPath, updated)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second reload expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	current, err := manager.Current()
	if err != nil {
		t.Fatalf("manager.Current: %v", err)
	}
	if len(current.Pools) != 1 || current.Pools[0].Name != "filepool" {
		t.Fatalf("store pools must survive file change, got %#v", current.Pools)
	}
	names, err := poolNames(poolStore)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	for _, n := range names {
		if strings.EqualFold(n, "otherpool") {
			t.Fatalf("file-only pool must not be migrated when bucket is non-empty, got %v", names)
		}
	}
}

// TestHandlerMetricsAndHealthzExposeCapacity 验收③（议题 #15）：admin 观测端点
// 暴露每节点在途数/容量/利用率。用真实转发中的一次请求验证运行态非零。
func TestHandlerMetricsAndHealthzExposeCapacity(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unlockUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer unlockUpstream() // 防断言提前退出时转发 goroutine 泄漏
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 2, []string{"k1"})
	cfg.Targets[0].Endpoint = upstream.URL
	cfg.Targets[0].MaxConcurrency = 1
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	clients := testClients([]string{"k1"})
	clients[0].IPAddress = "" // Capacity test uses an ephemeral loopback listener, not a fixed client IP.
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	service.SetUsageRecorder(permanentUsageFailureRecorder{})

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)

	authed := chi.NewRouter()
	authed.Use(auth.Middleware(store, logger))
	authed.Mount("/", service)
	proxySrv := httptest.NewServer(authed)
	defer proxySrv.Close()

	type fwdResult struct {
		status int
		body   string
		err    error
	}
	done := make(chan fwdResult, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
			bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer k1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- fwdResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		done <- fwdResult{status: resp.StatusCode, body: string(b)}
	}()
	select {
	case <-arrived:
	case r := <-done:
		t.Fatalf("request finished without reaching upstream: status=%d body=%q err=%v", r.status, r.body, r.err)
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received forwarded request (timeout)")
	}

	// /admin/metrics: targets_runtime 节点级明细。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/metrics status %d body %s", rec.Code, rec.Body.String())
	}
	var mResp struct {
		TargetsRuntime []struct {
			Name           string  `json:"name"`
			InFlight       int64   `json:"in_flight"`
			MaxConcurrency int     `json:"max_concurrency"`
			Utilization    float64 `json:"utilization"`
		} `json:"targets_runtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mResp); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	var t1m *struct {
		InFlight       int64
		MaxConcurrency int
		Utilization    float64
	}
	for i := range mResp.TargetsRuntime {
		ts := &mResp.TargetsRuntime[i]
		if ts.Name == "target-1" {
			t1m = &struct {
				InFlight       int64
				MaxConcurrency int
				Utilization    float64
			}{ts.InFlight, ts.MaxConcurrency, ts.Utilization}
		}
	}
	if t1m == nil {
		t.Fatalf("target-1 missing from targets_runtime: %+v", mResp.TargetsRuntime)
	}
	if t1m.InFlight != 1 || t1m.MaxConcurrency != 1 || t1m.Utilization != 1 {
		t.Fatalf("target-1 runtime = %+v, want in_flight=1 cap=1 util=1", t1m)
	}

	// /healthz: targets 数组携带容量字段。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil))
	var hResp struct {
		Targets []proxy.TargetStatus `json:"targets"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&hResp); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	var t1h *proxy.TargetStatus
	for i := range hResp.Targets {
		if hResp.Targets[i].Name == "target-1" {
			t1h = &hResp.Targets[i]
		}
	}
	if t1h == nil {
		t.Fatal("target-1 missing from healthz targets")
	}
	if t1h.InFlight != 1 || t1h.Capacity != 1 || t1h.Utilization != 1 {
		t.Fatalf("healthz target-1 = %+v, want in-flight 1 capacity 1 utilization 1", t1h)
	}

	unlockUpstream()
	if r := <-done; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("forwarded request failed: status=%d err=%v body=%s", r.status, r.err, r.body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if service.TargetStatuses(time.Now())[0].InFlight == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := service.TargetStatuses(time.Now())[0].InFlight; got != 0 {
		t.Fatalf("in-flight after completion = %d, want 0", got)
	}
	// A completed request leaves a recent peak without occupying real capacity.
	recentRec := httptest.NewRecorder()
	h.ServeHTTP(recentRec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
	var recentResponse struct {
		TargetsRuntime []metricsTargetRuntime `json:"targets_runtime"`
		Requests       struct {
			UsageRecordFailures int64 `json:"usage_record_failures"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(recentRec.Body.Bytes(), &recentResponse); err != nil {
		t.Fatal(err)
	}
	if recentResponse.Requests.UsageRecordFailures != 1 {
		t.Fatalf("/admin/metrics usage_record_failures = %d, want 1", recentResponse.Requests.UsageRecordFailures)
	}
	overviewRec := httptest.NewRecorder()
	h.ServeHTTP(overviewRec, httptest.NewRequest(http.MethodGet, "http://example.com/api/overview", nil))
	if overviewRec.Code != http.StatusOK {
		t.Fatalf("/admin/api/overview status %d body %s", overviewRec.Code, overviewRec.Body.String())
	}
	var overviewResponse struct {
		Requests map[string]int64 `json:"requests"`
	}
	if err := json.Unmarshal(overviewRec.Body.Bytes(), &overviewResponse); err != nil {
		t.Fatal(err)
	}
	if overviewResponse.Requests["usage_record_failures"] != 1 {
		t.Fatalf("/admin/api/overview usage_record_failures = %d, want 1", overviewResponse.Requests["usage_record_failures"])
	}
	// Zero-usage events for client disconnects are observable in both views.
	for _, key := range []string{"usage_client_gone_no_usage", "usage_upstream_interrupted_no_usage"} {
		if _, ok := overviewResponse.Requests[key]; !ok {
			t.Fatalf("/admin/api/overview missing %s", key)
		}
	}
	var recentRaw struct {
		Requests map[string]int64 `json:"requests"`
	}
	if err := json.Unmarshal(recentRec.Body.Bytes(), &recentRaw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"usage_client_gone_no_usage", "usage_upstream_interrupted_no_usage"} {
		if _, ok := recentRaw.Requests[key]; !ok {
			t.Fatalf("/admin/metrics missing %s", key)
		}
	}
	// The shared observation decode budget: capacity, bytes in use and
	// exhaustion counters per encoding.
	var budgetRaw struct {
		ObservationBudget *struct {
			CapacityBytes *int64 `json:"capacity_bytes"`
			InUseBytes    *int64 `json:"in_use_bytes"`
			Exhausted     *struct {
				Br   *int64 `json:"br"`
				Zstd *int64 `json:"zstd"`
			} `json:"observation_budget_exhausted"`
		} `json:"observation_budget"`
	}
	if err := json.Unmarshal(recentRec.Body.Bytes(), &budgetRaw); err != nil {
		t.Fatal(err)
	}
	if b := budgetRaw.ObservationBudget; b == nil || b.CapacityBytes == nil || *b.CapacityBytes <= 0 || b.InUseBytes == nil ||
		b.Exhausted == nil || b.Exhausted.Br == nil || b.Exhausted.Zstd == nil {
		t.Fatalf("/admin/metrics observation_budget incomplete: %s", recentRec.Body.String())
	}
	found := false
	for _, runtime := range recentResponse.TargetsRuntime {
		if runtime.Name != "target-1" {
			continue
		}
		found = true
		if runtime.InFlight != 0 || runtime.Window15m.InFlightPeak != 1 {
			t.Fatalf("real occupancy and recent peak conflated: %+v", runtime)
		}
	}
	if !found {
		t.Fatal("target-1 missing from recent metrics")
	}
}
