package admin

import (
	"context"
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

func traceTestHandler(t *testing.T, traceEnabled bool) *Handler {
	t.Helper()
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.TraceStore.Enabled = traceEnabled
	cfg.TraceStore.DiskPath = filepath.Join(tempDir, "trace.log")
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)
	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		t.Fatal(err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return NewHandler(config.NewManager(configPath), authStore, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, logger)
}

func traceGet(h *Handler, path string, ui bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
	if ui {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Issue #97: the shell needs trace enablement without scanning the trace store
// or shipping records, so startup is not gated on a disk-backed list.
func TestTraceStatsEndpointReportsEnablementOnly(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		h := traceTestHandler(t, enabled)
		rec := traceGet(h, "/data/trace/stats", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v status %d body %s", enabled, rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["records"]; ok {
			t.Fatalf("stats endpoint must not return records: %s", rec.Body.String())
		}
		var got bool
		if err := json.Unmarshal(body["enabled"], &got); err != nil || got != enabled {
			t.Fatalf("enabled = %s, want %v", body["enabled"], enabled)
		}
		if enabled {
			if _, ok := body["stats"]; !ok {
				t.Fatalf("enabled trace store must report stats: %s", rec.Body.String())
			}
		}
	}
	if rec := traceGet(traceTestHandler(t, true), "/data/trace/stats", false); rec.Code != http.StatusForbidden {
		t.Fatalf("stats without the admin UI header must be forbidden, got %d", rec.Code)
	}
}

// The list reports the limit it actually applied (invalid or oversized limits
// fall back to 50) so the UI pages by the server's value, and returns an empty
// array rather than null when there are no records.
func TestTraceListReportsEffectiveLimitAndOffset(t *testing.T) {
	h := traceTestHandler(t, true)
	for _, tc := range []struct {
		query  string
		limit  float64
		offset float64
	}{
		{"", 50, 0},
		{"?limit=20&offset=40", 20, 40},
		{"?limit=99999", 50, 0},
		{"?limit=abc&offset=-3", 50, 0},
	} {
		rec := traceGet(h, "/data/trace"+tc.query, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q status %d", tc.query, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["limit"] != tc.limit || body["offset"] != tc.offset {
			t.Errorf("%q: limit/offset = %v/%v, want %v/%v", tc.query, body["limit"], body["offset"], tc.limit, tc.offset)
		}
		if body["limit_max"] != float64(traceMaxLimit) {
			t.Errorf("%q: limit_max = %v, want %d so the UI can disclose the cap", tc.query, body["limit_max"], traceMaxLimit)
		}
		if records, ok := body["records"].([]any); !ok || len(records) != 0 {
			t.Errorf("%q: records = %#v, want empty array", tc.query, body["records"])
		}
		if body["view"] != "summary" {
			t.Errorf("%q: sessionless default view = %v, want summary", tc.query, body["view"])
		}
	}
	rec := traceGet(h, "/data/trace?view=full", true)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["view"]; ok {
		t.Errorf("view=full must not echo a summary view: %s", rec.Body.String())
	}
	if body["limit_max"] != float64(traceMaxLimit) {
		t.Errorf("view=full: limit_max = %v, want %d so every view discloses the cap", body["limit_max"], traceMaxLimit)
	}
	if records, ok := body["records"].([]any); !ok || len(records) != 0 {
		t.Errorf("view=full records = %#v, want empty array", body["records"])
	}
}

// Issue #97: usage reads run under the request context; a client abort stops
// the scan and is not reported as an internal server error.
func TestUsageReadHandlersStopOnClientAbort(t *testing.T) {
	h := traceTestHandler(t, false)
	for _, path := range []string{
		"/data/usage/aggregate?group_by=hour&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z",
		"/data/usage/aggregate?group_by=hour&model_group=g&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z",
		"/data/usage/summary",
		"/api/overview",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusInternalServerError || rec.Body.Len() != 0 {
			t.Errorf("%s: aborted request got status %d body %q; want no response written", path, rec.Code, rec.Body.String())
		}
	}
}
