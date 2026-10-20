package admin

import (
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageAPIIncludesProviderPartitionsAndCoverage(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"test-key"})
	event := usage.Event{Timestamp: time.Now().UTC(), ClientName: "client", Target: "target-1", Model: "model", StatusCode: 200, InputTokens: 10, OutputTokens: 8, CachedTokens: 5, CacheCreationTokens: 7, ExplicitCacheReadTokens: 3, ReasoningTokens: 2, CacheCreation5mTokens: 4, CacheCreation1hTokens: 3, ToolUsePromptTokens: 9, UsageStatus: usage.UsageStatusIncomplete, CostIncomplete: true}
	if err := stores.usageStore.Record(event); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/data/usage/aggregate", "/data/usage/summary"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", route, rec.Code, rec.Body.String())
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		key := "totals"
		if route == "/data/usage/summary" {
			key = "today"
		}
		var totals usage.Totals
		if err := json.Unmarshal(result[key], &totals); err != nil {
			t.Fatal(err)
		}
		if totals.ExplicitCacheReadTokens != 3 || totals.ReasoningTokens != 2 || totals.CacheCreation5mTokens != 4 || totals.CacheCreation1hTokens != 3 || totals.ToolUsePromptTokens != 9 || totals.IncompleteUsageCount != 1 || totals.IncompleteCostCount != 1 {
			t.Fatalf("%s lost fields: %+v", route, totals)
		}
	}
}
