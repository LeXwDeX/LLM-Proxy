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

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestUsageHandlersExposeAndAggregateIterationComponents(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	const targetName = "component-target"
	const mainModel = "main-model"
	const advisorModel = "advisor-model"
	if err := stores.targetStore.Create(config.Target{
		Name: targetName, EndpointType: "openai", UsageAdapter: "standard",
		Endpoint: "http://mock-a:9000", APIKey: "synthetic-key",
		ModelMappings: []config.ModelMapping{{Upstream: mainModel}, {Upstream: advisorModel}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, group := range []config.ModelGroup{
		{Name: "main-group", CostCoefficient: 0.5, Bindings: []config.QuotaGroupBinding{{Target: targetName, Model: mainModel}}},
		{Name: "advisor-group", CostCoefficient: 2, Bindings: []config.QuotaGroupBinding{{Target: targetName, Model: advisorModel}}},
	} {
		if err := stores.modelGroupStore.Create(group); err != nil {
			t.Fatal(err)
		}
	}
	cache := pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{
			{ID: "main-price", Target: targetName, Model: mainModel, Mode: pricing.ModeFlat,
				BaseRates: usage.CostRates{InputPer1MTokens: 2}},
			{ID: "advisor-price", Target: targetName, Model: advisorModel, Mode: pricing.ModeFlat,
				BaseRates: usage.CostRates{InputPer1MTokens: 5}},
		}, nil
	}, nil)
	h := &Handler{
		usageStore: stores.usageStore, targetStore: stores.targetStore,
		modelGroupStore: stores.modelGroupStore, costCache: cache,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	event := usage.Event{
		Timestamp: now, ClientName: "alice", EndpointType: "openai", RequestID: "request-components-1",
		Target: targetName, Path: "/v1/messages", Model: mainModel, StatusCode: http.StatusOK,
		InputTokens: 1_000_000, UsageStatus: usage.UsageStatusComplete,
		Components: []usage.Component{
			{Model: advisorModel, IterationType: "advisor_message", IterationIndex: 1,
				InputTokens: 2_000_000, UsageStatus: usage.UsageStatusComplete},
			{Model: mainModel, IterationType: "compaction", IterationIndex: 2, NonBillable: true,
				InputTokens: 7, UsageStatus: usage.UsageStatusComplete},
		},
	}
	usage.CheckBundlePrices(&event, h.getResolver())
	if err := stores.usageStore.Record(event); err != nil {
		t.Fatal(err)
	}

	from, to := now.Add(-time.Minute), now.Add(time.Minute)
	window := "?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	h.handleListUsageEvents(rec, httptest.NewRequest(http.MethodGet, "/admin/data/usage/events"+window+"&limit=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Events []usage.Event `json:"events"`
		Count  int           `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Count != 3 || len(listed.Events) != 3 {
		t.Fatalf("events should contain one root and two component rows: count=%d rows=%d", listed.Count, len(listed.Events))
	}
	var root *usage.Event
	rowsByModel := map[string][]usage.Event{}
	for i := range listed.Events {
		row := listed.Events[i]
		if row.RequestID != event.RequestID {
			t.Fatalf("row request_id=%q want %q", row.RequestID, event.RequestID)
		}
		rowsByModel[row.Model] = append(rowsByModel[row.Model], row)
		if !row.Supplementary {
			root = &listed.Events[i]
		}
	}
	if root == nil || len(root.Components) != 2 {
		t.Fatalf("root event did not expose component evidence: %+v", root)
	}
	if root.RecordID == "" || len(rowsByModel[mainModel]) != 2 || len(rowsByModel[advisorModel]) != 1 {
		t.Fatalf("component model attribution mismatch: main=%d advisor=%d", len(rowsByModel[mainModel]), len(rowsByModel[advisorModel]))
	}
	for _, row := range listed.Events {
		if row.Supplementary && (row.ParentRecordID != root.RecordID || row.RequestID != root.RequestID) {
			t.Fatalf("supplementary linkage mismatch: row=%+v root=%+v", row, root)
		}
	}
	var compaction *usage.Event
	for i := range listed.Events {
		if listed.Events[i].IterationType == "compaction" {
			compaction = &listed.Events[i]
		}
	}
	if compaction == nil || !compaction.NonBillable || compaction.InputTokens != 7 {
		t.Fatalf("non-billable compaction usage was not preserved: %+v", compaction)
	}

	query := window + "&group_by=day"
	rec = httptest.NewRecorder()
	h.handleAggregateUsage(rec, httptest.NewRequest(http.MethodGet, "/admin/data/usage/aggregate"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
	}
	var aggregate usage.AggregateResult
	if err := json.Unmarshal(rec.Body.Bytes(), &aggregate); err != nil {
		t.Fatal(err)
	}
	assertComponentTotals(t, "aggregate", aggregate.Totals, 1, 21, 3_000_007)
	models := map[string]usage.Totals{}
	for _, row := range aggregate.ByModelGroupModel {
		models[row.Model] = row.Totals
	}
	if len(models) != 2 {
		t.Fatalf("aggregate model rows=%+v", aggregate.ByModelGroupModel)
	}
	assertComponentTotals(t, "main model", models[mainModel], 1, 1, 1_000_007)
	assertComponentTotals(t, "advisor model", models[advisorModel], 0, 20, 2_000_000)

	rec = httptest.NewRecorder()
	h.handleUsageSummary(rec, httptest.NewRequest(http.MethodGet, "/admin/data/usage/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status=%d body=%s", rec.Code, rec.Body.String())
	}
	var summary usage.SummaryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	assertComponentTotals(t, "summary", summary.Today, 1, 21, 3_000_007)

	rec = httptest.NewRecorder()
	h.handleUsageModelGroups(rec, httptest.NewRequest(http.MethodGet, "/admin/data/usage/model-groups"+window, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("model groups status=%d body=%s", rec.Code, rec.Body.String())
	}
	var groups usage.ModelGroupUsageResult
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	groupTotals := map[string]usage.Totals{}
	groupModels := map[string]usage.Totals{}
	for _, group := range groups.Groups {
		groupTotals[group.Name] = group.Totals
		for _, row := range group.ByTargetModel {
			groupModels[group.Name+"/"+row.Model] = row.Totals
		}
	}
	assertComponentTotals(t, "main group", groupTotals["main-group"], 1, 1, 1_000_007)
	assertComponentTotals(t, "advisor group", groupTotals["advisor-group"], 0, 20, 2_000_000)
	if len(groupModels) != 2 {
		t.Fatalf("model-group model ownership=%v", groupModels)
	}
	assertComponentTotals(t, "main group model", groupModels["main-group/"+mainModel], 1, 1, 1_000_007)
	assertComponentTotals(t, "advisor group model", groupModels["advisor-group/"+advisorModel], 0, 20, 2_000_000)
}

func assertComponentTotals(t *testing.T, name string, totals usage.Totals, requests int64, cost float64, input int64) {
	t.Helper()
	if totals.Requests != requests || totals.EstimatedCost != cost || totals.InputTokens != input {
		t.Errorf("%s totals: requests=%d cost=%g input=%d; want requests=%d cost=%g input=%d", name,
			totals.Requests, totals.EstimatedCost, totals.InputTokens, requests, cost, input)
	}
}
