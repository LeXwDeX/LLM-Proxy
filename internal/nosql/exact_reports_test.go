package nosql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func minuteReportFixture(t *testing.T) (*UsageStore, *pricing.Resolver, time.Time) {
	t.Helper()
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{29*time.Minute + 59*time.Second, 30 * time.Minute, 45 * time.Minute} {
		code := 200
		if i == 2 {
			code = 500
		}
		if err := s.Record(usage.Event{Timestamp: day.Add(10*time.Hour + offset), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: code}); err != nil {
			t.Fatal(err)
		}
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{
		{Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
		{Target: "upstream", Model: "qwen-max", Start: day.Add(10*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
	}, nil)
	return s, r, day
}

func TestMinuteReportsUseEventPricesAndReuseDimensions(t *testing.T) {
	s, r, day := minuteReportFixture(t)
	to := day.Add(11 * time.Hour)
	filter := usage.Filter{From: &day, To: &to}
	summary, err := s.Summary(to, r)
	if err != nil || summary.Today.EstimatedCost != 21 || summary.LastHour.EstimatedCost != 21 {
		t.Fatalf("summary=%+v err=%v; want 21", summary, err)
	}
	for _, success := range []bool{false, true} {
		got, err := s.aggregateTargetModelCells(filter, "hour", r, nil, success)
		if err != nil {
			t.Fatal(err)
		}
		wantRequests := int64(3)
		if success {
			wantRequests = 2
		}
		if got.Totals.EstimatedCost != 21 || got.Totals.Requests != wantRequests {
			t.Fatalf("totals=%+v success=%v", got.Totals, success)
		}
		for name, total := range map[string]float64{"bucket": got.Buckets[0].EstimatedCost, "client": got.ByClient[0].EstimatedCost, "model": got.ByModel[0].EstimatedCost, "group-model": got.ByModelGroupModel[0].EstimatedCost, "target-model": got.ByTargetModel[0].EstimatedCost} {
			if total != 21 {
				t.Fatalf("%s cost=%v want 21", name, total)
			}
		}
	}
	groups, err := s.AggregateModelGroups(filter, r, nil)
	if err != nil || groups.Groups[0].Totals.EstimatedCost != 21 {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	plain, err := s.Aggregate(filter, "hour", r)
	if err != nil || plain.Totals.EstimatedCost != 21 || plain.Totals.Requests != 3 {
		t.Fatalf("legacy aggregate minute prices=%+v err=%v", plain, err)
	}
}

func TestMixedPriceReportDimensionsSumToTotal(t *testing.T) {
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := day.Add(48 * time.Hour)
	targets := NewTargetStore(db)
	for _, name := range []string{"target-a", "target-b"} {
		if err := targets.Create(config.Target{Name: name, Endpoint: "https://example.invalid", EndpointType: "openai", APIKey: "fixture-key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
			t.Fatal(err)
		}
	}
	groups := NewModelGroupStore(db)
	for _, g := range []config.ModelGroup{
		{Name: "group-a", CostCoefficient: 0, Bindings: []config.QuotaGroupBinding{{Target: "target-a"}}},
		{Name: "group-b", CostCoefficient: 0.25, Bindings: []config.QuotaGroupBinding{{Target: "target-b"}}},
	} {
		if err := groups.Create(g); err != nil {
			t.Fatal(err)
		}
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{
		{Target: "target-a", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
		{Target: "target-a", Model: "qwen-max", Start: day.Add(10*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
		{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 3}},
		{Model: "glm-flash", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}},
	}, nil)
	for _, e := range []usage.Event{
		{Timestamp: day.Add(10*time.Hour + 29*time.Minute + 59*time.Second), Target: "target-a", ClientName: "alice", Model: "qwen-max", StatusCode: 200},
		{Timestamp: day.Add(10*time.Hour + 30*time.Minute), Target: "target-a", ClientName: "bob", Model: "qwen-max", StatusCode: 200},
		{Timestamp: day.Add(10*time.Hour + 45*time.Minute), Target: "target-a", ClientName: "alice", Model: "qwen-max", StatusCode: 500},
		{Timestamp: day.Add(11*time.Hour + 45*time.Minute), Target: "target-b", ClientName: "bob", Model: "qwen-max", StatusCode: 200},
		{Timestamp: day.Add(34*time.Hour + 40*time.Minute), Target: "target-a", ClientName: "carol", Model: "glm-flash", StatusCode: 200},
	} {
		e.InputTokens = 1_000_000
		if err := s.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	scans, decoded := 0, 0
	got, err := s.aggregateTargetModelCellsRead(usage.Filter{From: &day, To: &to}, "day", r, targets, false, costReadHooks{scan: func(bool) { scans++ }, decoded: func() { decoded++ }}, groups)
	if err != nil || got.Totals.EstimatedCost != 29 || scans != 1 || decoded != 5 {
		t.Fatalf("mixed report=%+v scans=%d decoded=%d err=%v", got, scans, decoded, err)
	}
	sums := make(map[string]float64)
	for _, b := range got.Buckets {
		sums["buckets"] += b.EstimatedCost
	}
	for _, d := range got.ByClient {
		sums["clients"] += d.EstimatedCost
	}
	for _, d := range got.ByModel {
		sums["models"] += d.EstimatedCost
	}
	for _, d := range got.ByModelGroupModel {
		sums["group-models"] += d.EstimatedCost
	}
	for _, d := range got.ByTargetModel {
		sums["target-models"] += d.Totals.EstimatedCost
	}
	for dim, sum := range sums {
		if sum != got.Totals.EstimatedCost {
			t.Fatalf("%s sum=%v total=%v", dim, sum, got.Totals.EstimatedCost)
		}
	}
	filtered, err := s.AggregateByModelGroups(usage.Filter{From: &day, To: &to, ModelGroups: []string{"group-a"}}, "day", r, targets, groups)
	if err != nil || filtered.Totals.EstimatedCost != 26 || filtered.Totals.Requests != 3 {
		t.Fatalf("filtered report=%+v err=%v", filtered, err)
	}
}

func TestMinuteReportsRejectPrunedHistory(t *testing.T) {
	s, r, day := minuteReportFixture(t)
	to := day.Add(11 * time.Hour)
	if _, err := s.PruneEventsBefore(to, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AggregateByModelGroupsAllRequests(usage.Filter{From: &day, To: &to}, "hour", r, nil); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("pruned report returned guessed bill: %v", err)
	}
}

func TestModelGroupReportsScopeHistoryValidation(t *testing.T) {
	for _, path := range []string{"index", "full-fallback"} {
		for _, history := range []string{"pruned", "orphan-raw"} {
			t.Run(path+"/"+history, func(t *testing.T) {
				db := testDB(t)
				s := NewUsageStore(db)
				day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
				to := day.Add(12 * time.Hour)
				targets := NewTargetStore(db)
				if err := targets.Create(config.Target{Name: "shared", Endpoint: "https://example.invalid", APIKey: "fixture-key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: "qwen-max"}, {Upstream: "glm-flash"}}}); err != nil {
					t.Fatal(err)
				}
				groups := NewModelGroupStore(db)
				// Exact bindings on the same target require pair-level filtering.
				for _, g := range []config.ModelGroup{
					{Name: "group-a", Bindings: []config.QuotaGroupBinding{{Target: "shared", Model: "qwen-max"}}},
					{Name: "group-b", Bindings: []config.QuotaGroupBinding{{Target: "shared", Model: "glm-flash"}}},
				} {
					if err := groups.Create(g); err != nil {
						t.Fatal(err)
					}
				}
				r := pricing.NewResolver([]pricing.PricingTemplate{
					{Target: "shared", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
					{Target: "shared", Model: "glm-flash", Start: day.Add(11*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
				}, nil)
				for i, model := range []string{"qwen-max", "qwen-max", "qwen-max", "glm-flash"} {
					code := 200
					if i == 2 {
						code = 500
					}
					if err := s.Record(usage.Event{Timestamp: day.Add(time.Duration(10+i/3)*time.Hour + 45*time.Minute), ClientName: "alice", Target: "shared", Model: model, InputTokens: 1_000_000, StatusCode: code}); err != nil {
						t.Fatal(err)
					}
				}
				if history == "pruned" {
					if _, err := s.PruneEventsBefore(to, 10); err != nil {
						t.Fatal(err)
					}
				} else if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(day.Add(11*time.Hour), "alice", "shared", "glm-flash"))
				}); err != nil {
					t.Fatal(err)
				}
				migrateClientIndex(t, db)
				if path == "full-fallback" {
					if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageClientIndexDone)) }); err != nil {
						t.Fatal(err)
					}
				}
				for _, selection := range []struct {
					name   string
					groups []string
					fail   bool
				}{{"a", []string{"GROUP-A"}, false}, {"b", []string{"group-b"}, true}, {"all", nil, true}} {
					t.Run(selection.name, func(t *testing.T) {
						filter := usage.Filter{From: &day, To: &to, ClientName: "alice", ModelGroups: selection.groups}
						for _, entry := range []string{"AggregateByModelGroups", "AggregateByModelGroupsAllRequests", "AggregateModelGroups"} {
							t.Run(entry, func(t *testing.T) {
								var totals usage.Totals
								var err error
								wantRequests := int64(2)
								if entry == "AggregateModelGroups" {
									var got usage.ModelGroupUsageResult
									got, err = s.AggregateModelGroups(filter, r, targets, groups)
									if !selection.fail && err == nil {
										if len(got.Groups) != 1 || got.Groups[0].Name != "group-a" {
											t.Fatalf("groups=%+v", got)
										}
										totals = got.Groups[0].Totals
									}
									if err != nil && len(got.Groups) != 0 {
										t.Fatal("error returned partial model-group results")
									}
								} else {
									var got usage.AggregateResult
									if entry == "AggregateByModelGroups" {
										got, err = s.AggregateByModelGroups(filter, "hour", r, targets, groups)
									} else {
										wantRequests = 3
										got, err = s.AggregateByModelGroupsAllRequests(filter, "hour", r, targets, groups)
									}
									totals = got.Totals
									if err != nil && (got.Totals.Requests != 0 || len(got.Buckets) != 0 || len(got.ByTargetModel) != 0) {
										t.Fatal("error returned partial aggregate results")
									}
								}
								if selection.fail {
									if !errors.Is(err, ErrUsageHistoryIncomplete) {
										t.Fatalf("incomplete selected history accepted: %v", err)
									}
								} else if err != nil || totals.EstimatedCost != 6 || totals.Requests != wantRequests {
									t.Fatalf("stable selected group totals=%+v err=%v; want cost=6 requests=%d", totals, err, wantRequests)
								}
							})
						}
					})
				}
			})
		}
	}
}

type reportOwnershipChangeResolver struct {
	usage.RateResolver
	onPrice func()
}

func (r *reportOwnershipChangeResolver) RateFor(target, model string, at time.Time) (usage.CostRates, bool) {
	if r.onPrice != nil {
		change := r.onPrice
		r.onPrice = nil
		change()
	}
	return r.RateResolver.RateFor(target, model, at)
}

func (r *reportOwnershipChangeResolver) RequiresEventTimestamps() bool {
	return usage.RequiresEventTimestamps(r.RateResolver)
}

func TestModelGroupReportsReuseOwnershipSnapshot(t *testing.T) {
	for _, entry := range []string{"AggregateByModelGroups", "AggregateModelGroups"} {
		t.Run(entry, func(t *testing.T) {
			db := testDB(t)
			s := NewUsageStore(db)
			targets := NewTargetStore(db)
			if err := targets.Create(config.Target{Name: "shared", Endpoint: "https://example.invalid", APIKey: "fixture-key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
				t.Fatal(err)
			}
			groups := NewModelGroupStore(db)
			group := config.ModelGroup{Name: "group-a", Bindings: []config.QuotaGroupBinding{{Target: "shared"}}}
			if err := groups.Create(group); err != nil {
				t.Fatal(err)
			}
			from := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
			to := from.Add(time.Hour)
			if err := s.Record(usage.Event{Timestamp: from.Add(45 * time.Minute), ClientName: "alice", Target: "shared", Model: "qwen-max", StatusCode: 200, InputTokens: 1_000_000}); err != nil {
				t.Fatal(err)
			}
			changed := false
			r := &reportOwnershipChangeResolver{RateResolver: usage.CostTable{"qwen-max": {InputPer1MTokens: 2}}, onPrice: func() {
				// Pricing runs after history validation and outside the read tx.
				// A catalog change here must not reclassify report detail rows.
				group.Name = "group-b"
				if _, err := groups.Update("group-a", group); err != nil {
					t.Fatal(err)
				}
				changed = true
			}}
			filter := usage.Filter{From: &from, To: &to, ModelGroups: []string{"group-a"}}
			var totals usage.Totals
			var dimensions []usage.ModelGroupModelTotals
			if entry == "AggregateByModelGroups" {
				got, err := s.AggregateByModelGroups(filter, "hour", r, targets, groups)
				if err != nil {
					t.Fatal(err)
				}
				totals, dimensions = got.Totals, got.ByModelGroupModel
			} else {
				got, err := s.AggregateModelGroups(filter, r, targets, groups)
				if err != nil || len(got.Groups) != 1 || got.Groups[0].Name != "group-a" {
					t.Fatalf("groups=%+v err=%v", got, err)
				}
				totals, dimensions = got.Groups[0].Totals, got.ByModelGroupModel
			}
			if !changed || totals.Requests != 1 || totals.EstimatedCost != 2 || len(dimensions) != 1 || dimensions[0].ModelGroup != "group-a" || dimensions[0].EstimatedCost != 2 {
				t.Fatalf("ownership snapshot changed: changed=%v totals=%+v dimensions=%+v", changed, totals, dimensions)
			}
		})
	}
}

func TestIndexedExactCostsDecodeOnlySelectedClientAndFallBackCompletely(t *testing.T) {
	s, r, day := minuteReportFixture(t)
	to := day.Add(11 * time.Hour)
	for i := 0; i < 30; i++ {
		if err := s.Record(usage.Event{Timestamp: day.Add(10 * time.Hour), ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, s.db)
	filter := usage.Filter{From: &day, To: &to, ClientName: "alice"}
	decoded, indexed, full := 0, 0, 0
	hooks := costReadHooks{decoded: func() { decoded++ }, scan: func(index bool) {
		if index {
			indexed++
		} else {
			full++
		}
	}}
	indexedData, err := s.readCostUnits(context.Background(), filter, r, true, nil, hooks)
	if err != nil || decoded != 3 || indexed != 1 || full != 0 {
		t.Fatalf("indexed decode=%d reads=%d full=%d err=%v", decoded, indexed, full, err)
	}
	for _, problem := range []string{"incomplete", "quarantine", "corrupt-index"} {
		t.Run(problem, func(t *testing.T) {
			if err := s.db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				if problem == "incomplete" {
					return meta.Delete([]byte(usageClientIndexDone))
				}
				if problem == "quarantine" {
					return meta.Put([]byte(usageQuarantineCountKey), []byte("unresolved"))
				}
				c := tx.Bucket([]byte(BucketUsageEventsByClient)).Cursor()
				k, _ := c.Seek([]byte(clientIndexPrefix("alice")))
				return tx.Bucket([]byte(BucketUsageEventsByClient)).Put(k, []byte("missing-raw"))
			}); err != nil {
				t.Fatal(err)
			}
			decoded, indexed, full = 0, 0, 0
			got, err := s.readCostUnits(context.Background(), filter, r, true, nil, hooks)
			if err != nil || full != 1 || !reflect.DeepEqual(got.units, indexedData.units) {
				t.Fatalf("fallback differs: full=%d err=%v got=%+v", full, err, got.units)
			}
			if err := s.db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				if err := meta.Delete([]byte(usageQuarantineCountKey)); err != nil {
					return err
				}
				return meta.Put([]byte(usageClientIndexDone), []byte("complete"))
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExactReportOneRawScanForAllDimensionsAndSummary(t *testing.T) {
	s, r, day := minuteReportFixture(t)
	to := day.Add(11 * time.Hour)
	scans, decoded := 0, 0
	hooks := costReadHooks{scan: func(bool) { scans++ }, decoded: func() { decoded++ }}
	got, err := s.aggregateTargetModelCellsRead(usage.Filter{From: &day, To: &to}, "hour", r, nil, false, hooks)
	if err != nil || scans != 1 || decoded != 3 || got.Totals.EstimatedCost != 21 {
		t.Fatalf("all report dimensions scans=%d decoded=%d err=%v", scans, decoded, err)
	}
	scans, decoded = 0, 0
	summary, err := s.summaryRead(to, r, hooks)
	if err != nil || scans != 1 || decoded != 3 || summary.LastHour.EstimatedCost != 21 || summary.Last30Days.EstimatedCost != 21 {
		t.Fatalf("summary scans=%d decoded=%d summary=%+v err=%v", scans, decoded, summary, err)
	}
}

func TestExactReportPreservesHourBoundsWhileQuotaUsesFractionalEventTime(t *testing.T) {
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	from := day.Add(10*time.Hour + 30*time.Minute + 120*time.Millisecond)
	to := from.Add(3 * time.Millisecond)
	events := []usage.Event{}
	for _, offset := range []time.Duration{-time.Nanosecond, 0, 2 * time.Millisecond, 3 * time.Millisecond, time.Second} {
		events = append(events, usage.Event{Timestamp: from.Add(offset), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000})
	}
	putLegacyClientEvents(t, db, events)
	migrateClientIndex(t, db)
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: from, Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil)
	filter := usage.Filter{From: &from, To: &to, ClientName: "alice"}
	quota, err := s.SumTargetModelCostsRange(filter, r)
	if err != nil || len(quota) != 1 || quota[0].CostUSD != 6 {
		t.Fatalf("fractional inclusive quota=%+v err=%v", quota, err)
	}
	for _, index := range []bool{true, false} {
		got, err := s.readCostUnits(context.Background(), filter, r, false, nil, costReadHooks{forceLegacy: !index})
		var cost float64
		for _, u := range got.units {
			cost += u.CostUSD
		}
		if err != nil || cost != 8 {
			t.Fatalf("hour-window report cost=%v index=%v err=%v", cost, index, err)
		}
	}
}

func TestExactCostsRejectOrphanRawWithoutRollup(t *testing.T) {
	s, r, day := minuteReportFixture(t)
	to := day.Add(11 * time.Hour)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		k, _ := b.Cursor().First()
		return b.Delete(k)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SumTargetModelCostsRange(usage.Filter{From: &day, To: &to, ClientName: "alice"}, r); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("orphan raw cost accepted: %v", err)
	}
}

func TestCostUnitsKeepTemplateFallbackZeroAndProviderCatalogPrices(t *testing.T) {
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := day.Add(11 * time.Hour)
	cat, err := catalog.New()
	if err != nil {
		t.Fatal(err)
	}
	cache := pricing.NewTargetPricingCache(func() ([]pricing.PricingTemplate, error) {
		return []pricing.PricingTemplate{
			{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
			{Model: "qwen-max", Start: day.Add(10*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 4}},
			{Target: "specific", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
			{Target: "specific", Model: "qwen-max", Start: day.Add(10*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}},
			{Target: "zero", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{}},
		}, nil
	}, cat, func() (map[string]string, error) {
		return map[string]string{"openai-target": "openai", "azure-target": "azure_openai"}, nil
	})
	r, err := cache.GetChecked()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"specific", "global", "zero", "openai-target", "azure-target"} {
		model := "qwen-max"
		if target == "openai-target" || target == "azure-target" {
			model = "gpt-4"
		}
		for _, offset := range []time.Duration{29*time.Minute + 59*time.Second, 30 * time.Minute, 45 * time.Minute} {
			if err := s.Record(usage.Event{Timestamp: day.Add(10*time.Hour + offset), ClientName: "alice", Target: target, Model: model, InputTokens: 1_000_000, StatusCode: 200}); err != nil {
				t.Fatal(err)
			}
		}
	}
	migrateClientIndex(t, db)
	got, err := s.SumTargetModelCostsRange(usage.Filter{From: &day, To: &to, ClientName: "alice"}, r)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"specific": 21, "global": 10, "zero": 0,
		"openai-target": 3 * cat.LookupDefaultCost("openai", "gpt-4").InputPer1MTokens,
		"azure-target":  3 * cat.LookupDefaultCost("azure_openai", "gpt-4").InputPer1MTokens}
	for _, c := range got {
		if c.CostUSD != want[c.Target] {
			t.Fatalf("fallback target=%s got=%v want=%v", c.Target, c.CostUSD, want[c.Target])
		}
		delete(want, c.Target)
	}
	if len(want) != 0 {
		t.Fatalf("missing cost dimensions: %v", want)
	}
	// A provider-scoped catalog miss must remain unpriced, not inherit a
	// different provider's same-name price from the generic catalog fallback.
	if _, ok := r.RateFor("azure-target", "gpt-does-not-exist", day); ok {
		t.Fatal("provider miss received a fallback price")
	}
	if err := s.Record(usage.Event{Timestamp: day.Add(10 * time.Hour), ClientName: "alice", Target: "azure-target", Model: "gpt-does-not-exist", InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	modelFilter := usage.Filter{From: &day, To: &to, ClientName: "alice", Model: "gpt-does-not-exist"}
	miss, err := s.SumTargetModelCostsRange(modelFilter, r)
	if err != nil || len(miss) != 1 || miss[0].CostUSD != 0 {
		t.Fatalf("provider miss billed: %+v %v", miss, err)
	}
}

func BenchmarkExactCostConsumers(b *testing.B) {
	db := testDB(b)
	s := NewUsageStore(db)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * 24 * time.Hour)
	const clients, perClient = 40, 1000
	events := make([]usage.Event, 0, clients*perClient)
	for c := 0; c < clients; c++ {
		for i := 0; i < perClient; i++ {
			events = append(events, usage.Event{Timestamp: from.Add(time.Duration(i) * 43 * time.Minute), ClientName: fmt.Sprintf("client-%02d", c), Target: "upstream", Model: "qwen-max", InputTokens: 100, StatusCode: 200})
		}
	}
	putLegacyClientEvents(b, db, events)
	if err := MigrateUsageClientIndex(context.Background(), db, 4096); err != nil {
		b.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: from.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil)
	for _, mode := range []string{"evaluate_old_three_full_scans", "evaluate_union_client_index", "report_old_hourly_two_reads", "report_legacy_exact_two_scans", "report_exact_one_scan"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			decoded, scans := 0, 0
			hooks := costReadHooks{decoded: func() { decoded++ }, scan: func(bool) { scans++ }, forceLegacy: true}
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				filter := usage.Filter{From: &from, To: &to}
				switch mode {
				case "evaluate_old_three_full_scans":
					filter.ClientName = "client-00"
					for _, days := range []int{1, 7, 30} {
						start := to.Add(-time.Duration(days) * 24 * time.Hour)
						filter.From = &start
						if _, err := s.readCostUnits(context.Background(), filter, r, true, nil, hooks); err != nil {
							b.Fatal(err)
						}
					}
				case "evaluate_union_client_index":
					filter.ClientName = "client-00"
					hooks.forceLegacy = false
					if _, err := s.readCostUnits(context.Background(), filter, r, true, nil, hooks); err != nil {
						b.Fatal(err)
					}
				case "report_old_hourly_two_reads":
					for i := 0; i < 2; i++ {
						cells, err := s.SumTargetModelRange(filter)
						if err != nil {
							b.Fatal(err)
						}
						for _, c := range cells {
							_, _ = aggCellCost(c.AggCell, c.Target, c.Hour, c.Model, r)
						}
					}
				case "report_legacy_exact_two_scans":
					for i := 0; i < 2; i++ {
						if _, err := s.readCostUnits(context.Background(), filter, r, false, nil, hooks); err != nil {
							b.Fatal(err)
						}
					}
				case "report_exact_one_scan":
					if _, err := s.aggregateTargetModelCellsRead(filter, "day", r, nil, false, hooks); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(decoded)/float64(b.N), "decoded/op")
			b.ReportMetric(float64(scans)/float64(b.N), "raw-scans/op")
		})
	}
}
