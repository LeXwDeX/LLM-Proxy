package quota

import (
	"context"
	"errors"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"math"
	"strings"
	"testing"
	"time"
)

func TestImageAccountingReportsAndEvaluate(t *testing.T) {
	for _, precise := range []bool{false, true} {
		t.Run(map[bool]string{false: "hourly", true: "minute template"}[precise], func(t *testing.T) {
			db := testOpenDB(t)
			putClient(t, db, config.Client{Name: "alice", AccessKey: "key", JobLevel: "L5"})
			putJobLevel(t, db, config.JobLevel{Name: "L5"})
			putModelGroup(t, db, config.ModelGroup{Name: "group", CostCoefficient: .5, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "target"}}})
			putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "group"}})
			image, cache, out := 10.0, 3.0, 20.0
			rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 1, ImageInputPer1MTokens: &image, CachedImageInputPer1MTokens: &cache, ImageOutputPer1MTokens: &out}
			now := time.Now().UTC()
			tmpl := pricing.PricingTemplate{Target: "target", Model: "model", Mode: pricing.ModeFlat, BaseRates: rates}
			if precise {
				tmpl.Start = now.Truncate(time.Hour).Add(-time.Minute)
			}
			m := newTestManager(t, db)
			m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) { return []pricing.PricingTemplate{tmpl}, nil }, nil)
			m.Evaluate("alice")
			evt := usage.Event{Timestamp: now, ClientName: "alice", Target: "target", Model: "model", StatusCode: 200, UsageStatus: usage.UsageStatusComplete, InputTokens: 1000000, OutputTokens: 1000000, CachedTokens: 1000000, ImageInputTokens: 200000, CachedImageInputTokens: 300000, ImageOutputTokens: 400000}
			if err := m.RecordUsage(m.usageStore, evt); err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				if got := m.Status("alice").Quotas["daily"].Used; math.Abs(got-7.8) > 1e-10 {
					t.Fatalf("quota=%v", got)
				}
			}
			check()
			m.Evaluate("alice")
			check()
			m.Evaluate("alice")
			check()
			from, to := now.Truncate(time.Hour), now.Add(time.Second)
			filter := usage.Filter{From: &from, To: &to, ClientName: "alice"}
			resolver := usage.WithCoefficient(m.costCache.Get(), func(string, string) float64 { return .5 })
			units, err := m.usageStore.ReadCostUnits(context.Background(), filter, resolver, true)
			if err != nil || len(units) != 1 || units[0].EventPriced != precise || math.Abs(units[0].CostUSD-7.8) > 1e-10 {
				t.Fatalf("units=%+v err=%v", units, err)
			}
			for _, u := range units {
				if u.ImageInputTokens != 200000 || u.CachedImageInputTokens != 300000 || u.ImageOutputTokens != 400000 {
					t.Fatalf("lost partitions %+v", u)
				}
			}
			agg, err := m.usageStore.AggregateByModelGroupsAllRequests(filter, "hour", resolver, nosql.NewTargetStore(db), nosql.NewModelGroupStore(db))
			if err != nil || math.Abs(agg.Totals.EstimatedCost-7.8) > 1e-10 || agg.Totals.ImageOutputTokens != 400000 || agg.Totals.IncompleteCostCount != 0 {
				t.Fatalf("aggregate=%+v err=%v", agg, err)
			}
			summary, err := m.usageStore.Summary(to, resolver)
			if err != nil || math.Abs(summary.Today.EstimatedCost-7.8) > 1e-10 || summary.Today.CachedImageInputTokens != 300000 {
				t.Fatalf("summary=%+v err=%v", summary, err)
			}
			events, err := m.usageStore.List(filter)
			if err != nil || len(events) != 1 || events[0].ImageInputTokens != 200000 || events[0].CostIncomplete {
				t.Fatalf("events=%+v err=%v", events, err)
			}
		})
	}
}

func TestImageOverageAccountingIsLinear(t *testing.T) {
	for _, precise := range []bool{false, true} {
		t.Run(map[bool]string{false: "hourly", true: "exact"}[precise], func(t *testing.T) {
			db := testOpenDB(t)
			putClient(t, db, config.Client{Name: "alice", AccessKey: "key", JobLevel: "L5"})
			putJobLevel(t, db, config.JobLevel{Name: "L5"})
			putModelGroup(t, db, config.ModelGroup{Name: "group", CostCoefficient: 1, DailyUSD: 100, Bindings: []config.QuotaGroupBinding{{Target: "target"}}})
			putClientQuotaGroups(t, db, "alice", []config.QuotaGroup{{Name: "group"}})
			image := 10.0
			now := time.Now().UTC().Add(-time.Second)
			tmpl := pricing.PricingTemplate{Target: "target", Model: "model", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2, ImageInputPer1MTokens: &image}}
			if precise {
				tmpl.Start = now.Truncate(time.Hour).Add(-time.Minute)
			}
			m := newTestManager(t, db)
			m.costCache = pricing.NewPricingCache(func() ([]pricing.PricingTemplate, error) { return []pricing.PricingTemplate{tmpl}, nil }, nil)
			m.Evaluate("alice")
			for i, images := range []int64{2000000, 0} {
				evt := usage.Event{Timestamp: now.Add(time.Duration(i) * time.Millisecond), ClientName: "alice", Target: "target", Model: "model", StatusCode: 200, UsageStatus: usage.UsageStatusComplete, InputTokens: 1000000, ImageInputTokens: images}
				if err := m.RecordUsage(m.usageStore, evt); err != nil {
					t.Fatal(err)
				}
			}
			check := func(stage string) {
				t.Helper()
				if got := m.Status("alice").Quotas["daily"].Used; math.Abs(got-12) > 1e-10 {
					t.Fatalf("%s quota=%v want=12", stage, got)
				}
			}
			check("increment")
			m.Evaluate("alice")
			check("Evaluate")
			from, to := now.Truncate(time.Hour), now.Add(time.Second)
			filter := usage.Filter{From: &from, To: &to, ClientName: "alice"}
			units, err := m.usageStore.ReadCostUnits(context.Background(), filter, m.costCache.Get(), true)
			if err != nil || len(units) == 0 {
				t.Fatalf("units=%+v err=%v", units, err)
			}
			var cost float64
			var images int64
			for _, u := range units {
				cost += u.CostUSD
				images += u.ImageInputTokens
				if u.EventPriced != precise || u.ImageInputTokens > u.InputTokens {
					t.Fatalf("unit=%+v", u)
				}
			}
			if cost != 12 || images != 1000000 {
				t.Fatalf("units=%+v", units)
			}
			events, err := m.usageStore.List(filter)
			if err != nil || len(events) != 2 {
				t.Fatalf("events=%+v err=%v", events, err)
			}
			for _, e := range events {
				if e.ImageInputTokens > e.InputTokens || e.ImageInputTokens > 0 && (!e.CostIncomplete || !strings.Contains(e.UsageDiagnostic, "image_partition_exceeds_total")) {
					t.Fatalf("event=%+v", e)
				}
			}
			if n, err := m.usageStore.PruneEventsBefore(to.Add(2*time.Second), 10); err != nil || n != 2 {
				t.Fatalf("pruned=%d err=%v", n, err)
			}
			m.Evaluate("alice")
			check("Evaluate after prune")
			if precise {
				// Preserve P2's fail-closed rule: exact prices need raw events.
				if _, err := m.usageStore.ReadCostUnits(context.Background(), filter, m.costCache.Get(), true); !errors.Is(err, nosql.ErrUsageHistoryIncomplete) {
					t.Fatalf("missing exact history: %v", err)
				}
				m.mu.RLock()
				blocked := m.calibrationErrors["alice"] != ""
				m.mu.RUnlock()
				if !blocked {
					t.Fatal("exact calibration did not fail closed after prune")
				}
			}
		})
	}
}
