package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func float64Ptr(v float64) *float64 { return &v }

func assertClientTotalsMatchAggregate(t *testing.T, got usage.ClientUsageSummary, want usage.AggregateResult) {
	t.Helper()
	if !got.Exact || got.Source != usage.ClientUsageSourceAggregate {
		t.Fatalf("exact=%v source=%q, want aggregate path", got.Exact, got.Source)
	}
	if !got.From.Equal(want.From) || !got.To.Equal(want.To) {
		t.Fatalf("window=%v..%v want %v..%v", got.From, got.To, want.From, want.To)
	}
	if !reflect.DeepEqual(got.Clients, want.ByClient) {
		t.Fatalf("clients mismatch:\n got=%+v\nwant=%+v", got.Clients, want.ByClient)
	}
}

func TestAggregateClientTotalsMatchesAggregateByClient(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := day.Add(24 * time.Hour)
	filter := usage.Filter{From: &day, To: &to}

	type scenario struct {
		name      string
		events    []usage.Event
		resolver  usage.RateResolver
		targets   func(t *testing.T, db *bolt.DB) *TargetStore
		wantCosts map[string]float64
	}
	scenarios := []scenario{
		{
			name: "flat prices",
			events: []usage.Event{
				{Timestamp: day.Add(10*time.Hour + 5*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
				{Timestamp: day.Add(10*time.Hour + 45*time.Minute), ClientName: "bob", Target: "upstream", Model: "glm-flash", InputTokens: 2_000_000, StatusCode: 500},
				{Timestamp: day.Add(11*time.Hour + 30*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 3_000_000, StatusCode: 200},
			},
			resolver: pricing.NewResolver([]pricing.PricingTemplate{
				{Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
				{Target: "upstream", Model: "glm-flash", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 5}},
			}, nil),
			wantCosts: map[string]float64{"alice": 8, "bob": 10},
		},
		{
			name: "peak and off-peak schedule",
			events: []usage.Event{
				{Timestamp: day.Add(1*time.Hour + 30*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
				{Timestamp: day.Add(9*time.Hour + 30*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
				{Timestamp: day.Add(9*time.Hour + 40*time.Minute), ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
			},
			resolver: pricing.NewResolver([]pricing.PricingTemplate{{
				Target: "upstream", Model: "qwen-max", Mode: pricing.ModeSchedule, Timezone: "+00:00",
				Periods: []pricing.PricingPeriod{
					{Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: "00:00", End: "08:00", Rates: usage.CostRates{InputPer1MTokens: 1}},
					{Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: "08:00", End: "24:00", Rates: usage.CostRates{InputPer1MTokens: 4}},
				},
			}}, nil),
			wantCosts: map[string]float64{"alice": 5, "bob": 4},
		},
		{
			name: "cache read and write subsets",
			events: []usage.Event{
				{
					Timestamp: day.Add(10*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max",
					InputTokens: 1_000_000, OutputTokens: 400_000, CachedTokens: 400_000, CacheCreationTokens: 200_000,
					ExplicitCacheReadTokens: 100_000, CacheCreation5mTokens: 50_000, CacheCreation1hTokens: 30_000,
					StatusCode: 200,
				},
				{
					Timestamp: day.Add(10*time.Hour + 20*time.Minute), ClientName: "bob", Target: "upstream", Model: "qwen-max",
					InputTokens: 2_000_000, CachedTokens: 500_000, StatusCode: 200,
				},
			},
			resolver: pricing.NewResolver([]pricing.PricingTemplate{{
				Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat,
				BaseRates: usage.CostRates{
					InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheWritePer1MToken: 1.5, CacheReadPer1MToken: 0.5,
					ExplicitCacheReadPer1MToken: float64Ptr(0.75), CacheWrite5mPer1MToken: float64Ptr(1.25), CacheWrite1hPer1MToken: float64Ptr(2.5),
				},
			}}, nil),
		},
		{
			name: "zero coefficient free target",
			events: []usage.Event{
				{Timestamp: day.Add(10*time.Hour + 10*time.Minute), ClientName: "alice", Target: "paid-target", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
				{Timestamp: day.Add(10*time.Hour + 20*time.Minute), ClientName: "bob", Target: "free-target", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
			},
			resolver: usage.WithCoefficient(
				pricing.NewResolver([]pricing.PricingTemplate{
					{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 3}},
				}, nil),
				func(target, _ string) float64 {
					if target == "free-target" {
						return 0
					}
					return 1
				},
			),
			wantCosts: map[string]float64{"alice": 3, "bob": 0},
		},
		{
			name: "history kept by renamed target",
			events: []usage.Event{
				{Timestamp: day.Add(10*time.Hour + 10*time.Minute), ClientName: "alice", Target: "old-name", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
				{Timestamp: day.Add(10*time.Hour + 20*time.Minute), ClientName: "bob", Target: "old-name", Model: "qwen-max", InputTokens: 2_000_000, StatusCode: 200},
			},
			resolver: pricing.NewResolver([]pricing.PricingTemplate{
				{Target: "old-name", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
			}, nil),
			targets: func(t *testing.T, db *bolt.DB) *TargetStore {
				t.Helper()
				targets := NewTargetStore(db)
				if err := targets.Create(config.Target{Name: "old-name", Endpoint: "https://example.invalid", EndpointType: "openai", APIKey: "fixture-key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
					t.Fatal(err)
				}
				return targets
			},
			wantCosts: map[string]float64{"alice": 2, "bob": 4},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			db := testDB(t)
			s := NewUsageStore(db)
			var targets *TargetStore
			if sc.targets != nil {
				targets = sc.targets(t, db)
			}
			for _, event := range sc.events {
				if err := s.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			if sc.targets != nil {
				if _, err := targets.Rename(TargetRenameRequest{From: "old-name", To: "new-name"}); err != nil {
					t.Fatalf("rename fixture: %v", err)
				}
			}
			want, err := s.AggregateByModelGroupsAllRequestsContext(context.Background(), filter, "day", sc.resolver, targets)
			if err != nil {
				t.Fatalf("aggregate by client: %v", err)
			}
			got, err := s.AggregateClientTotalsContext(context.Background(), filter, sc.resolver, targets)
			if err != nil {
				t.Fatalf("client totals: %v", err)
			}
			assertClientTotalsMatchAggregate(t, got, want)
			for key, cost := range sc.wantCosts {
				found := false
				for _, client := range got.Clients {
					if client.Key == key {
						found = true
						if client.EstimatedCost != cost {
							t.Fatalf("client %s cost=%v want %v", key, client.EstimatedCost, cost)
						}
					}
				}
				if !found {
					t.Fatalf("client %s missing from report: %+v", key, got.Clients)
				}
			}
		})
	}
}

func TestAggregateClientTotalsFallsBackForEventPricedPlans(t *testing.T) {
	s, r, day := minuteReportFixture(t)
	for i := 0; i < 2; i++ {
		if err := s.Record(usage.Event{Timestamp: day.Add(10*time.Hour + 15*time.Minute), ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	to := day.Add(11 * time.Hour)
	filter := usage.Filter{From: &day, To: &to}
	want, err := s.AggregateByModelGroupsAllRequestsContext(context.Background(), filter, "day", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	scans, decoded := 0, 0
	hooks := costReadHooks{scan: func(bool) { scans++ }, decoded: func() { decoded++ }}
	got, err := s.aggregateClientTotalsContext(context.Background(), filter, r, nil, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exact || got.Source != usage.ClientUsageSourceEvents {
		t.Fatalf("event-priced fallback exact=%v source=%q", got.Exact, got.Source)
	}
	if !reflect.DeepEqual(got.Clients, want.ByClient) {
		t.Fatalf("fallback clients mismatch:\n got=%+v\nwant=%+v", got.Clients, want.ByClient)
	}
	if scans != 1 || decoded != 5 {
		t.Fatalf("fallback scans=%d decoded=%d, want one event scan of 5 rows", scans, decoded)
	}
}

func TestAggregateClientTotalsReadsOnlyHourlyRollups(t *testing.T) {
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := day.Add(24 * time.Hour)
	events := []usage.Event{
		{Timestamp: day.Add(10*time.Hour + 5*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
		{Timestamp: day.Add(10*time.Hour + 6*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200},
		{Timestamp: day.Add(11*time.Hour + 6*time.Minute), ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000, StatusCode: 200},
	}
	putLegacyClientEvents(t, db, events)
	// A malformed raw row must not affect a reader that never opens the events
	// bucket; only the hourly rollups above are consulted.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(usageEventKeyForTest(events[0]), []byte("not-json"))
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{
		{Target: "upstream", Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}},
	}, nil)
	filter := usage.Filter{From: &day, To: &to}
	decoded, scans, directRaw := 0, 0, 0
	hooks := costReadHooks{
		decoded: func() { decoded++ },
		scan:    func(bool) { scans++ },
		direct:  &directIntegrityHooks{raw: func() { directRaw++ }, call: func(time.Time, time.Time) { scans++ }},
	}
	got, err := s.aggregateClientTotalsContext(context.Background(), filter, r, nil, hooks)
	if err != nil {
		t.Fatalf("hourly-only report failed on malformed raw event: %v", err)
	}
	if decoded != 0 || scans != 0 || directRaw != 0 {
		t.Fatalf("raw event reads decoded=%d scans=%d raw=%d, want 0", decoded, scans, directRaw)
	}
	want := []usage.DimensionTotals{
		{Key: "alice", Totals: usage.Totals{Requests: 2, InputTokens: 2_000_000, EstimatedCost: 4}},
		{Key: "bob", Totals: usage.Totals{Requests: 1, InputTokens: 2_000_000, EstimatedCost: 4}},
	}
	if !reflect.DeepEqual(got.Clients, want) {
		t.Fatalf("clients=%+v want %+v", got.Clients, want)
	}
	// The exported reader keeps the same behavior without test hooks.
	if exported, err := s.AggregateClientTotalsContext(context.Background(), filter, r, nil); err != nil || !reflect.DeepEqual(exported.Clients, want) {
		t.Fatalf("exported reader clients=%+v err=%v", exported.Clients, err)
	}
	// Contrast: the strict report path inspects raw-event evidence and rejects
	// this dataset, which is exactly what the lightweight reader avoids.
	if _, err := s.AggregateByModelGroupsAllRequestsContext(context.Background(), filter, "day", r, nil); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("strict aggregate accepted malformed raw evidence: %v", err)
	}
}

func usageEventKeyForTest(event usage.Event) []byte {
	return []byte(usageKey(event.Timestamp, "0"))
}

func TestAggregateClientTotalsRejectsMissingAggregateSource(t *testing.T) {
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := day.Add(time.Hour)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageAggregateSourceMissingPrefix+BucketQuotaUsageAggHourly), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	filter := usage.Filter{From: &day, To: &to}
	r := usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}
	_, err := s.AggregateClientTotalsContext(context.Background(), filter, r, nil)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("missing aggregate source accepted: %v", err)
	}
	if _, err := s.AggregateByModelGroupsAllRequestsContext(context.Background(), filter, "day", r, nil); !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("fixture does not reproduce aggregate rejection: %v", err)
	}
}

func TestAggregateClientTotalsMapsEmptyClientToUnknown(t *testing.T) {
	db := testDB(t)
	s := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := day.Add(24 * time.Hour)
	cell, err := json.Marshal(AggCell{RecordCount: 1, PriceCoverageRecordCount: 1, Requests: 2, InputTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(quotaAggKey(day.Add(10*time.Hour), "", "upstream", "qwen-max"), cell)
	}); err != nil {
		t.Fatal(err)
	}
	r := usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}
	got, err := s.AggregateClientTotalsContext(context.Background(), usage.Filter{From: &day, To: &to}, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []usage.DimensionTotals{{Key: "unknown", Totals: usage.Totals{Requests: 2, InputTokens: 5, EstimatedCost: 5.0 / 1_000_000}}}
	if !reflect.DeepEqual(got.Clients, want) {
		t.Fatalf("clients=%+v want %+v", got.Clients, want)
	}
}

func BenchmarkClientUsageSummary(b *testing.B) {
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
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil)
	filter := usage.Filter{From: &from, To: &to}
	cells := 0
	if err := db.View(func(tx *bolt.Tx) error {
		rollups, err := readCostRollups(context.Background(), tx, filter, from, to, nil)
		cells = len(rollups)
		return err
	}); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"aggregate_by_client_old", "clients_lightweight"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			decoded, scans, directRaw := 0, 0, 0
			hooks := costReadHooks{
				decoded: func() { decoded++ },
				scan:    func(bool) { scans++ },
				direct:  &directIntegrityHooks{raw: func() { directRaw++ }, call: func(time.Time, time.Time) {}},
			}
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				var err error
				if mode == "aggregate_by_client_old" {
					_, err = s.aggregateTargetModelCellsReadContext(context.Background(), filter, "day", r, nil, false, hooks)
				} else {
					_, err = s.aggregateClientTotalsContext(context.Background(), filter, r, nil, hooks)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(decoded)/float64(b.N), "decoded/op")
			b.ReportMetric(float64(scans)/float64(b.N), "scan-calls/op")
			b.ReportMetric(float64(directRaw)/float64(b.N), "direct-raw-visits/op")
			b.ReportMetric(float64(cells), "rollup-cells")
		})
	}
}
