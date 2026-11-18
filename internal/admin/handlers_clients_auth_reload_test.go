package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// newAuthReloadFailureHandler builds a handler whose client store commits
// normally but whose auth reload always fails: it is constructed without an
// auth store, so LoadFromConfigWithPools reports "auth store is nil" on every
// attempt.
func newAuthReloadFailureHandler(t *testing.T, logBuf *bytes.Buffer) (http.Handler, testStores) {
	t.Helper()
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	manager := config.NewManager(configPath)
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, nil, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	return h, stores
}

func requireAuthReloadPending(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if payload["code"] != "auth_reload_pending" {
		t.Fatalf("code = %q, want auth_reload_pending (body=%s)", payload["code"], rec.Body.String())
	}
	if payload["error"] != "client saved but auth reload pending" {
		t.Fatalf("error = %q, want the pending message", payload["error"])
	}
}

// A committed create whose auth reload fails must report a pending reload
// (not 201) and retry the reload a bounded number of times.
func TestCreateClientReportsAuthReloadPendingAfterRetries(t *testing.T) {
	var logs bytes.Buffer
	h, stores := newAuthReloadFailureHandler(t, &logs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients",
		bytes.NewBufferString(`{"name":"bob","access_key":"bob-key"}`)))
	requireAuthReloadPending(t, rec)

	if stored, err := stores.clientStore.Get("bob"); err != nil || stored == nil {
		t.Fatalf("client write must stay committed, got %+v err=%v", stored, err)
	}
	if got := strings.Count(logs.String(), "auth reload attempt failed"); got != 3 {
		t.Fatalf("auth reload attempts = %d, want 3 (logs=%s)", got, logs.String())
	}
}

func TestUpdateClientReportsAuthReloadPending(t *testing.T) {
	var logs bytes.Buffer
	h, stores := newAuthReloadFailureHandler(t, &logs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/clients/client-1",
		bytes.NewBufferString(`{"name":"client-1","access_key":"k1-new","ip_address":"192.0.2.1"}`)))
	requireAuthReloadPending(t, rec)

	stored, err := stores.clientStore.Get("client-1")
	if err != nil || stored == nil || stored.AccessKey != "k1-new" {
		t.Fatalf("client update must stay committed, got %+v err=%v", stored, err)
	}
}

func TestDeleteClientReportsAuthReloadPending(t *testing.T) {
	var logs bytes.Buffer
	h, stores := newAuthReloadFailureHandler(t, &logs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/clients/client-1", nil))
	requireAuthReloadPending(t, rec)

	if stored, err := stores.clientStore.Get("client-1"); err != nil || stored != nil {
		t.Fatalf("client deletion must stay committed, got %+v err=%v", stored, err)
	}
}
