package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// TestOverviewCountIncludesLegacyAmbiguousAggregate is the direct consumer
// regression for the management overview. A legacy key with a delimiter-bearing
// client stays undecodable for dimensions but must still contribute to the
// 72-hour request and success totals.
func TestOverviewCountIncludesLegacyAmbiguousAggregate(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
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

	hour := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	if err := stores.usageStore.Record(usage.Event{
		Timestamp: hour.Add(10 * time.Minute), ClientName: "alice", Model: "model",
		EndpointType: "openai", Target: "upstream", StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	legacyKey := hour.Format("2006-01-02T15") + "|openai|team|api|model|upstream"
	if err := stores.db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(nosql.AggCell{RecordCount: 2, Requests: 2, Success: 1})
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(nosql.BucketUsageAggHourly)).Put([]byte(legacyKey), data)
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/api/overview status %d body %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Requests72h int64 `json:"requests_72h"`
		Success72h  int64 `json:"success_72h"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Requests72h != 3 || response.Success72h != 2 {
		t.Fatalf("overview 72h=(%d,%d), want (3,2)", response.Requests72h, response.Success72h)
	}
}

// TestOverviewCountSkipsCorruptAggregate keeps the management overview
// available when an upgraded ledger still holds an undecodable hourly cell
// that v1.0.1 readers skipped (#35). The display total excludes the cell.
func TestOverviewCountSkipsCorruptAggregate(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
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

	hour := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	if err := stores.usageStore.Record(usage.Event{
		Timestamp: hour.Add(10 * time.Minute), ClientName: "alice", Model: "model",
		EndpointType: "openai", Target: "upstream", StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	corruptKey := hour.Format("2006-01-02T15") + "|openai|c|m|t"
	if err := stores.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketUsageAggHourly)).Put([]byte(corruptKey), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/api/overview status %d body %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Requests72h int64 `json:"requests_72h"`
		Success72h  int64 `json:"success_72h"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Requests72h != 1 || response.Success72h != 1 {
		t.Fatalf("overview 72h=(%d,%d), want (1,1)", response.Requests72h, response.Success72h)
	}
}
