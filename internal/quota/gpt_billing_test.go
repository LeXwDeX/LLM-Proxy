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

func TestGPTCacheBillingPersonalGroupPoolAndReports(t *testing.T) {
	for _, provider := range []string{"openai", "azure_openai"} {
		t.Run(provider, func(t *testing.T) {
			db := testOpenDB(t)
			putClient(t, db, config.Client{Name: "alice", AccessKey: "key", JobLevel: "L5"})
			putJobLevel(t, db, config.JobLevel{Name: "L5"})
			putModelGroup(t, db, config.ModelGroup{Name: "gpt", CostCoefficient: 0.5, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "target"}}})
			putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "gpt"}})
			rates := usage.CostRates{InputPer1MTokens: 4, OutputPer1MTokens: 20, CacheReadPer1MToken: 0.4, CacheWritePer1MToken: 5}
			m := newTestManager(t, db)
			m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) {
				return []pricing.PricingTemplate{{Target: "target", Model: "gpt-5.6-sol", BaseRates: rates}}, nil
			}, nil)
			m.Evaluate("alice")
			event := usage.Event{Timestamp: time.Now().UTC(), ClientName: "alice", Target: "target", EndpointType: provider, Model: "gpt-5.6-sol", InputTokens: 1000, OutputTokens: 300, CachedTokens: 12000, CacheCreationTokens: 2000, StatusCode: 200}
			if err := m.usageStore.Record(event); err != nil {
				t.Fatal(err)
			}
			m.IncrementForTarget("alice", "target", provider, event.Model, event.Timestamp, 1000, 300, 12000, 2000)
			check := func(stage string) {
				t.Helper()
				status := m.Status("alice")
				// 额度 v2：组维度按折算口径（原价 × 组系数 0.5）累计；
				// plan 聚合视图与组同币种。
				for name, got := range map[string]float64{"aggregate": status.Quotas["daily"].Used, "group": status.Groups["gpt"].Quotas["daily"].Used} {
					want := 0.0248 * 0.5
					if math.Abs(got-want) > 1e-10 {
						t.Fatalf("%s %s=%g want %g", stage, name, got, want)
					}
				}
			}
			check("increment")
			m.Evaluate("alice")
			check("recalibration")
			from := event.Timestamp.Truncate(time.Hour)
			to := from.Add(time.Hour)
			resolver := usage.WithCoefficient(m.costCache.Get(), func(string, string) float64 { return 0.5 })
			agg, err := m.usageStore.AggregateByModelGroupsAllRequests(usage.Filter{From: &from, To: &to}, "hour", resolver, nosql.NewTargetStore(db), nosql.NewModelGroupStore(db))
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(agg.Totals.EstimatedCost-0.0124) > 1e-10 || agg.Totals.CacheCreationTokens != 2000 {
				t.Fatalf("discounted report=%+v", agg.Totals)
			}
		})
	}
}
