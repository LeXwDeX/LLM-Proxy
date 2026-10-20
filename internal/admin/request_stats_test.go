package admin

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestOverviewRequestStatsIndependentEmpty(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	service, err := proxy.NewService(&config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetRequestStatsRecorder(nosql.NewRequestStatsStore(stores.clientStore.DB()))
	h := NewHandler(nil, nil, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, nil)
	w := httptest.NewRecorder()
	h.handleOverview(w, httptest.NewRequest("GET", "/admin/api/overview", nil))
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	stats, ok := data["request_stats_72h"].(map[string]any)
	if !ok {
		t.Fatalf("independent request statistics missing: %s", w.Body.String())
	}
	if stats["available"] != true || stats["total"] != float64(0) || stats["success_rate"] != nil {
		t.Fatalf("empty stats=%v", stats)
	}
	for _, key := range []string{"from", "to", "since", "sample_basis", "exclusions", "record_failures"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("missing %s: %v", key, stats)
		}
	}
	if data["legacy_sample_basis"] != "usage_events" {
		t.Errorf("legacy basis=%v", data["legacy_sample_basis"])
	}
}

func TestOverviewRequestStatsAbsentRecorderUnavailable(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	service, err := proxy.NewService(&config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	h := NewHandler(nil, nil, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, nil)
	w := httptest.NewRecorder()
	h.handleOverview(w, httptest.NewRequest("GET", "/admin/api/overview", nil))
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	stats := data["request_stats_72h"].(map[string]any)
	if stats["available"] != false || stats["success_rate"] != nil || stats["error"] == nil {
		t.Fatalf("absent recorder reported available: %v", stats)
	}
}

type unavailableRequestStats struct{}

func (unavailableRequestStats) Snapshot(time.Time) (requeststats.Snapshot, error) {
	return requeststats.Snapshot{}, errors.New("read failed")
}
func (unavailableRequestStats) Record(requeststats.Event) error { return errors.New("write failed") }

func TestOverviewRequestStatsMetadataAndUnavailable(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	service, err := proxy.NewService(&config.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	h := NewHandler(nil, nil, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, nil, nil)
	now := time.Now().UTC()
	statsStore := nosql.NewRequestStatsStore(stores.clientStore.DB())
	for _, outcome := range []requeststats.Outcome{requeststats.Success, requeststats.Failure, requeststats.Canceled} {
		if err := statsStore.Record(requeststats.Event{Timestamp: now.Truncate(time.Minute).Add(-time.Minute), Outcome: outcome}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stores.usageStore.Record(usage.Event{Timestamp: now.Add(-time.Hour), StatusCode: 200, ClientName: "test", Model: "test", Target: "test"}); err != nil {
		t.Fatal(err)
	}
	service.SetRequestStatsRecorder(unavailableRequestStats{})
	service.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))
	read := func() map[string]any {
		w := httptest.NewRecorder()
		h.handleOverview(w, httptest.NewRequest("GET", "/admin/api/overview", nil))
		if w.Code != 200 {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
		var data map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := read()
	stats := data["request_stats_72h"].(map[string]any)
	if stats["total"] != float64(3) || stats["success"] != float64(1) || stats["failure"] != float64(1) || stats["canceled"] != float64(1) || stats["record_failures"] != float64(1) || stats["success_rate"] != float64(1)/3*100 {
		t.Fatalf("stats=%v", stats)
	}
	from, _ := time.Parse(time.RFC3339Nano, stats["from"].(string))
	to, _ := time.Parse(time.RFC3339Nano, stats["to"].(string))
	if to.Sub(from) != 72*time.Hour || to.Second() != 0 || to.Nanosecond() != 0 {
		t.Fatalf("window=%v %v", from, to)
	}
	if data["requests_72h"] != float64(1) || data["success_rate"] != float64(100) {
		t.Fatalf("legacy changed: %v", data)
	}
	for _, reader := range []requeststats.Reader{nil, unavailableRequestStats{}} {
		h.requestStatsStore = reader
		stats = read()["request_stats_72h"].(map[string]any)
		if stats["available"] != false || stats["success_rate"] != nil || stats["error"] == nil {
			t.Fatalf("unavailable hidden: %v", stats)
		}
	}
	// An installed reader and historical counters do not imply live collection.
	h.requestStatsStore = statsStore
	service.SetRequestStatsRecorder(nil)
	stats = read()["request_stats_72h"].(map[string]any)
	if stats["available"] != false || stats["success_rate"] != nil || stats["error"] == nil || stats["record_failures"] != float64(1) {
		t.Fatalf("removed recorder reported historical stats as available: %v", stats)
	}
}
