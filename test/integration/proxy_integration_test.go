//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/admin"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/logging"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

func TestEndToEndAdminAndProxyFlow(t *testing.T) {
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "integration-success"})
	}))
	defer success.Close()

	tempDir := t.TempDir()
	clients := []config.Client{{
		Name:      "integration",
		AccessKey: "integration-token",
		IPAddress: "192.0.2.1",
	}}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://127.0.0.1:1",
				ResourcePathPrefix: "/",
				APIKey:             "primary-key",
			},
			{
				Name:               "secondary",
				Endpoint:           success.URL,
				ResourcePathPrefix: "/",
				APIKey:             "secondary-key",
			},
		},
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
			AccessLog: filepath.Join(tempDir, "access.log"),
			ErrorLog:  filepath.Join(tempDir, "error.log"),
		},
	}

	logManager, err := logging.Setup(logging.Config{
		Level:         cfg.Logging.Level,
		AccessLogPath: cfg.Logging.AccessLog,
		ErrorLogPath:  cfg.Logging.ErrorLog,
	})
	if err != nil {
		t.Fatalf("logging setup: %v", err)
	}
	defer logManager.Close()

	appLogger := logManager.App()

	// Open bbolt DB and create stores.
	db, err := nosql.OpenDB(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	clientStore := nosql.NewClientStore(db)
	targetStore := nosql.NewTargetStore(db)
	modelCostStore := nosql.NewModelCostStore(db)
	usageStore := nosql.NewUsageStore(db)
	userStore := nosql.NewUserStore(db)
	auditStore := nosql.NewAuditStore(db)

	// Seed clients.
	for _, c := range clients {
		if err := clientStore.Create(c); err != nil {
			t.Fatalf("seed client: %v", err)
		}
	}

	proxyService, err := proxy.NewService(cfg, appLogger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("integration-token")
	if !ok {
		t.Fatalf("expected principal for integration token")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(`{"model":"mock-completion","prompt":"ping"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()

	proxyService.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}
	if resp.Header.Get("X-Azure-Target") != "secondary" {
		t.Fatalf("expected fallback to secondary target, header=%q", resp.Header.Get("X-Azure-Target"))
	}

	manager := config.NewManager("testdata/config.json")
	manager.Replace(cfg)

	adminHandler := admin.NewHandler(manager, store, proxyService, auditStore, userStore, clientStore, targetStore, modelCostStore, usageStore, nil, nil, nil, nil, appLogger)

	t.Run("health endpoint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}
		var payload struct {
			Targets []proxy.TargetStatus `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		if len(payload.Targets) != 2 {
			t.Fatalf("expected 2 targets, got %d", len(payload.Targets))
		}
	})

	t.Run("metrics endpoint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}
		var payload struct {
			Requests struct {
				Total   int64 `json:"total"`
				Success int64 `json:"success"`
			} `json:"requests"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode metrics: %v", err)
		}
		if payload.Requests.Total != 1 || payload.Requests.Success != 1 {
			t.Fatalf("unexpected request counters: %+v", payload.Requests)
		}
	})
}
