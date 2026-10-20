package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestUsageHandlersMinutePricesAndCountContracts(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{29*time.Minute + 59*time.Second, 30 * time.Minute, 45 * time.Minute} {
		code := 200
		if i == 2 {
			code = 500
		}
		if err := stores.usageStore.Record(usage.Event{Timestamp: day.Add(10*time.Hour + offset), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: code}); err != nil {
			t.Fatal(err)
		}
	}
	cache := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{
			{Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
			{Target: "upstream", Model: "qwen-max", Start: day.Add(10*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
		}, nil
	}, nil)
	h := &Handler{usageStore: stores.usageStore, targetStore: stores.targetStore, costCache: cache, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	query := "?from=" + url.QueryEscape(day.Format(time.RFC3339)) + "&to=" + url.QueryEscape(day.Add(11*time.Hour).Format(time.RFC3339))
	for _, filtered := range []bool{false, true} {
		path := query
		if filtered {
			path += "&model_group=company_general"
		}
		rec := httptest.NewRecorder()
		h.handleAggregateUsage(rec, httptest.NewRequest(http.MethodGet, "/usage/aggregate"+path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
		}
		var got usage.AggregateResult
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		requests := int64(3)
		if filtered {
			requests = 2
		}
		if got.Totals.EstimatedCost != 21 || got.Totals.Requests != requests || got.ByTargetModel[0].Totals.EstimatedCost != 21 {
			t.Fatalf("handler totals=%+v filtered=%v", got, filtered)
		}
	}
	rec := httptest.NewRecorder()
	h.handleUsageModelGroups(rec, httptest.NewRequest(http.MethodGet, "/usage/model-groups"+query, nil))
	var groups usage.ModelGroupUsageResult
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || groups.Groups[0].Totals.EstimatedCost != 21 || groups.Groups[0].Totals.Requests != 2 {
		t.Fatalf("group handler=%+v code=%d", groups, rec.Code)
	}
	rec = httptest.NewRecorder()
	h.handleUsageSummary(rec, httptest.NewRequest(http.MethodGet, "/usage/summary", nil))
	var summary usage.SummaryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || summary.Yesterday.EstimatedCost != 21 || summary.Yesterday.Requests != 3 {
		t.Fatalf("summary handler=%+v code=%d", summary, rec.Code)
	}
	if _, err := stores.usageStore.PruneEventsBefore(day.Add(24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.handleAggregateUsage(rec, httptest.NewRequest(http.MethodGet, "/usage/aggregate"+query, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing precise history returned successful bill: %d", rec.Code)
	}
}
