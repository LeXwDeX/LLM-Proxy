package quota

import (
	"math"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestProviderPartitionsSurviveAccountingAndRecalibration(t *testing.T) {
	db := testOpenDB(t)
	putClient(t, db, config.Client{Name: "alice", AccessKey: "key", JobLevel: "L5"})
	putJobLevel(t, db, config.JobLevel{Name: "L5"})
	putModelGroup(t, db, config.ModelGroup{Name: "group", CostCoefficient: 0.5, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "target"}}})
	putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "group"}})
	explicit, five, hour := 1.0, 5.0, 8.0
	rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 10, CacheReadPer1MToken: 2, CacheWritePer1MToken: 4, ExplicitCacheReadPer1MToken: &explicit, CacheWrite5mPer1MToken: &five, CacheWrite1hPer1MToken: &hour}
	m := newTestManager(t, db)
	m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{{Target: "target", Model: "model", BaseRates: rates}}, nil
	}, nil)
	m.Evaluate("alice")
	evt := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "target", Model: "model", StatusCode: 200, InputTokens: 1000000, OutputTokens: 100000, ReasoningTokens: 50000, CachedTokens: 1000000, ExplicitCacheReadTokens: 250000, CacheCreationTokens: 1000000, CacheCreation5mTokens: 200000, CacheCreation1hTokens: 300000, ToolUsePromptTokens: 42, UsageStatus: usage.UsageStatusComplete, UsageAdapter: "standard", CostIncomplete: true}
	if err := m.RecordUsage(m.usageStore, evt); err != nil {
		t.Fatal(err)
	}
	// 2 input + 1 output + 1.75 reads + 5.4 writes. Reasoning is an output subset.
	const raw = 10.15
	check := func() {
		t.Helper()
		s := m.Status("alice")
		// 额度 v2：组维度折算口径（× 组系数 0.5），聚合视图同币种。
		if math.Abs(s.Quotas["daily"].Used-raw/2) > 1e-10 || math.Abs(s.Groups["group"].Quotas["daily"].Used-raw/2) > 1e-10 {
			t.Fatalf("quota mismatch: %+v", s)
		}
	}
	check()
	m.Evaluate("alice")
	check()
	from, to := evt.Timestamp.Truncate(time.Hour), evt.Timestamp.Add(time.Hour)
	filter := usage.Filter{From: &from, To: &to}
	events, err := m.usageStore.List(filter)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0].ExplicitCacheReadTokens != 250000 || events[0].CacheCreation1hTokens != 300000 || !events[0].CostIncomplete {
		t.Fatalf("metadata lost: %+v", events[0])
	}
	agg, err := m.usageStore.AggregateByModelGroupsAllRequests(filter, "hour", usage.WithCoefficient(m.costCache.Get(), func(string, string) float64 { return .5 }), nosql.NewTargetStore(db), nosql.NewModelGroupStore(db))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(agg.Totals.EstimatedCost-raw/2) > 1e-10 || agg.Totals.ReasoningTokens != 50000 || agg.Totals.ToolUsePromptTokens != 42 || agg.Totals.IncompleteCostCount != 1 || agg.Totals.IncompleteUsageCount != 0 {
		t.Fatalf("report mismatch: %+v", agg.Totals)
	}
}
