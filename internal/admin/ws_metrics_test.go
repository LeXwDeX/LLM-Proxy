package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

// The upstream WebSocket pool counters, handshake phase histograms and the
// changed-header sample are visible in /admin/metrics.
func TestHandlerMetricsIncludesResponsesWebSocketPool(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatal(err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	h := NewHandler(config.NewManager(configPath), store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		ResponsesWS map[string]json.RawMessage `json:"responses_ws"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"hit_fresh", "hit_reuse_cached", "hit_continuation", "new_handshake", "native_handshake",
		"fallback_cap", "fallback_unsupported", "prewarm_dial_ok", "prewarm_dial_fail", "prewarm_used",
		"prewarm_discarded", "native_ping_fail", "handshake_dns_ms", "handshake_tcp_ms", "handshake_tls_ms",
		"handshake_upgrade_ms", "prewarm_handshake_dns_ms", "prewarm_handshake_tcp_ms", "prewarm_handshake_tls_ms",
		"prewarm_handshake_upgrade_ms", "header_change_samples", "header_changes",
	} {
		if _, ok := resp.ResponsesWS[key]; !ok {
			t.Errorf("/admin/metrics responses_ws missing %s: %s", key, rec.Body.String())
		}
	}
	var histogram struct {
		Count   int64 `json:"count"`
		Buckets []struct {
			LeMs  float64 `json:"le_ms"`
			Count int64   `json:"count"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(resp.ResponsesWS["handshake_tls_ms"], &histogram); err != nil || len(histogram.Buckets) == 0 {
		t.Fatalf("handshake_tls_ms histogram: %s %v", resp.ResponsesWS["handshake_tls_ms"], err)
	}
}
