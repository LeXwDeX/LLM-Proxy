package nosql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// exactCostTable forces every (target, model) through the event-priced path.
// The issue probe uses it to compare stable rollups with exact history.
type exactCostTable struct{ usage.CostTable }

func (exactCostTable) RequiresEventTimestamps() bool { return true }

func (exactCostTable) RequiresEventTimestampsFor(string, string) bool { return true }

func pricedIterationTable() usage.CostTable {
	return usage.CostTable{"root": {InputPer1MTokens: 2}, "advisor": {InputPer1MTokens: 2}}
}

func missingAdvisorTable() usage.CostTable {
	return usage.CostTable{"root": {InputPer1MTokens: 2}}
}

// iterationPricingFixture is the issue probe: a 1M root request, 2M billable
// advisor input, and 3M non-billable fallback input in one hour.
func iterationPricingFixture(at time.Time) []usage.Event {
	return []usage.Event{
		{Timestamp: at, ClientName: "alice", Target: "upstream", Model: "root", InputTokens: 1_000_000, StatusCode: 200},
		{Timestamp: at.Add(time.Minute), ClientName: "alice", Target: "upstream", Model: "advisor", InputTokens: 2_000_000, StatusCode: 200, Supplementary: true, IterationType: "advisor"},
		{Timestamp: at.Add(2 * time.Minute), ClientName: "alice", Target: "upstream", Model: "advisor", InputTokens: 3_000_000, StatusCode: 200, Supplementary: true, NonBillable: true, IterationType: "fallback_message"},
	}
}

func recordIterationPricingFixture(t *testing.T, store *UsageStore, at time.Time) []usage.Event {
	t.Helper()
	events := iterationPricingFixture(at)
	for _, event := range events {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

func TestStableAggregateReportsMissingSupplementaryIterationPrice(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 10, 0, 0, time.UTC)
	db := testDB(t)
	store := NewUsageStore(db)
	recordIterationPricingFixture(t, store, day)
	from, to := day.Truncate(time.Hour), day.Truncate(time.Hour).Add(time.Hour)

	stable, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", missingAdvisorTable())
	if err != nil {
		t.Fatal(err)
	}
	if stable.Totals.EstimatedCost != 2 || stable.Totals.IncompleteCostCount != 1 {
		t.Fatalf("stable cost=%.2f incomplete=%d, want cost=2 incomplete=1", stable.Totals.EstimatedCost, stable.Totals.IncompleteCostCount)
	}

	exact, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", exactCostTable{missingAdvisorTable()})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Totals.EstimatedCost != stable.Totals.EstimatedCost || exact.Totals.IncompleteCostCount != stable.Totals.IncompleteCostCount {
		t.Fatalf("exact cost=%.2f incomplete=%d differs from stable cost=%.2f incomplete=%d",
			exact.Totals.EstimatedCost, exact.Totals.IncompleteCostCount, stable.Totals.EstimatedCost, stable.Totals.IncompleteCostCount)
	}
}

func TestStableAggregateIterationPriceScope(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		events    []usage.Event
		resolver  usage.RateResolver
		wantCost  float64
		wantIncmp int64
	}{
		{
			name:     "ordinary root keeps legacy semantics",
			events:   []usage.Event{{Timestamp: day, ClientName: "alice", Target: "upstream", Model: "root", InputTokens: 1_000_000, StatusCode: 200}},
			resolver: usage.CostTable{}, wantCost: 0, wantIncmp: 0,
		},
		{
			name:     "fallback root is checked",
			events:   []usage.Event{{Timestamp: day, ClientName: "alice", Target: "upstream", Model: "fallback", IterationType: "fallback_message", InputTokens: 1_000_000, StatusCode: 200}},
			resolver: usage.CostTable{}, wantCost: 0, wantIncmp: 1,
		},
		{
			name:     "non-billable iteration is not a missing price",
			events:   []usage.Event{{Timestamp: day, ClientName: "alice", Target: "upstream", Model: "free", Supplementary: true, NonBillable: true, InputTokens: 1_000_000, StatusCode: 200}},
			resolver: usage.CostTable{}, wantCost: 0, wantIncmp: 0,
		},
		{
			name: "same model mixes billable and free iterations",
			events: []usage.Event{
				{Timestamp: day, ClientName: "alice", Target: "upstream", Model: "advisor", Supplementary: true, InputTokens: 2_000_000, StatusCode: 200},
				{Timestamp: day, ClientName: "alice", Target: "upstream", Model: "advisor", Supplementary: true, NonBillable: true, InputTokens: 3_000_000, StatusCode: 200},
			},
			resolver: usage.CostTable{}, wantCost: 0, wantIncmp: 1,
		},
		{
			name: "nil resolver is not a real missing price", events: iterationPricingFixture(day),
			resolver: nil, wantCost: 0, wantIncmp: 0,
		},
		{
			name: "explicit zero price is a valid price", events: iterationPricingFixture(day),
			resolver: usage.CostTable{"root": {}, "advisor": {}}, wantCost: 0, wantIncmp: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewUsageStore(testDB(t))
			for _, event := range tc.events {
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			from, to := day.Truncate(time.Hour), day.Truncate(time.Hour).Add(time.Hour)
			got, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", tc.resolver)
			if err != nil {
				t.Fatal(err)
			}
			if got.Totals.EstimatedCost != tc.wantCost || got.Totals.IncompleteCostCount != tc.wantIncmp {
				t.Fatalf("cost=%.2f incomplete=%d, want cost=%.2f incomplete=%d",
					got.Totals.EstimatedCost, got.Totals.IncompleteCostCount, tc.wantCost, tc.wantIncmp)
			}
		})
	}
}

func TestStableAggregatePreservesExistingIncompleteCount(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	store := NewUsageStore(testDB(t))
	event := usage.Event{Timestamp: day, ClientName: "alice", Target: "upstream", Model: "advisor",
		Supplementary: true, IterationType: "advisor", InputTokens: 2_000_000, StatusCode: 200, CostIncomplete: true}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := day, day.Add(time.Hour)
	got, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", usage.CostTable{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.IncompleteCostCount != 1 {
		t.Fatalf("incomplete=%d, want the persisted count once", got.Totals.IncompleteCostCount)
	}
}

func TestStableAggregatePriceRecoveryDoesNotPersistDerivedIncomplete(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	db := testDB(t)
	store := NewUsageStore(db)
	recordIterationPricingFixture(t, store, day)
	from, to := day.Truncate(time.Hour), day.Truncate(time.Hour).Add(time.Hour)

	missing, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", missingAdvisorTable())
	if err != nil || missing.Totals.IncompleteCostCount != 1 {
		t.Fatalf("missing price incomplete=%d err=%v, want 1", missing.Totals.IncompleteCostCount, err)
	}
	recovered, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", pricedIterationTable())
	if err != nil || recovered.Totals.IncompleteCostCount != 0 || recovered.Totals.EstimatedCost != 6 {
		t.Fatalf("recovered cost=%.2f incomplete=%d err=%v, want cost=6 incomplete=0",
			recovered.Totals.EstimatedCost, recovered.Totals.IncompleteCostCount, err)
	}

	var cell AggCell
	if err := db.View(func(tx *bolt.Tx) error {
		key := aggKey(day, "", "alice", "advisor", "upstream")
		return json.Unmarshal(tx.Bucket([]byte(BucketUsageAggHourly)).Get(key), &cell)
	}); err != nil {
		t.Fatal(err)
	}
	if cell.IncompleteCostCount != 0 {
		t.Fatalf("query-derived incomplete count was persisted: %+v", cell)
	}
	if cell.PriceCoverageRecordCount != 2 || cell.IterationPriceCompleteCount != 1 {
		t.Fatalf("coverage metadata=%+v, want coverage=2 complete=1", cell)
	}
}

func TestStableAggregateCoverageMetadata(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	db := testDB(t)
	store := NewUsageStore(db)
	recordIterationPricingFixture(t, store, day)

	cells := make(map[string]AggCell)
	if err := db.View(func(tx *bolt.Tx) error {
		for _, model := range []string{"root", "advisor"} {
			var cell AggCell
			if err := json.Unmarshal(tx.Bucket([]byte(BucketUsageAggHourly)).Get(aggKey(day, "", "alice", model, "upstream")), &cell); err != nil {
				return err
			}
			cells[model] = cell
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if cell := cells["advisor"]; cell.RecordCount != 2 || cell.PriceCoverageRecordCount != 2 || cell.IterationPriceCompleteCount != 1 {
		t.Fatalf("advisor coverage=%+v, want records=2 coverage=2 complete=1", cell)
	}
	if cell := cells["root"]; cell.RecordCount != 1 || cell.PriceCoverageRecordCount != 1 || cell.IterationPriceCompleteCount != 0 {
		t.Fatalf("root coverage=%+v, want records=1 coverage=1 complete=0", cell)
	}
}

func TestMixedLegacyAndNewCellIsNotAssumedCovered(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(AggCell{Requests: 1, InputTokens: 1_000_000})
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey(day, "", "alice", "root", "upstream"), data)
	}); err != nil {
		t.Fatal(err)
	}
	store := NewUsageStore(db)
	if err := store.Record(usage.Event{Timestamp: day.Add(time.Minute), ClientName: "alice", Target: "upstream", Model: "root", InputTokens: 1_000_000, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	var cell AggCell
	if err := db.View(func(tx *bolt.Tx) error {
		return json.Unmarshal(tx.Bucket([]byte(BucketUsageAggHourly)).Get(aggKey(day, "", "alice", "root", "upstream")), &cell)
	}); err != nil {
		t.Fatal(err)
	}
	if cell.RecordCount != 2 || cell.PriceCoverageRecordCount != 1 {
		t.Fatalf("mixed coverage=%+v, want records=2 coverage=1", cell)
	}
	from, to := day, day.Add(time.Hour)
	got, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", usage.CostTable{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.IncompleteCostCount != 1 {
		t.Fatalf("mixed cell incomplete=%d, want one conservative marker for the legacy row", got.Totals.IncompleteCostCount)
	}
}

func TestStablePriceCoverageSurvivesClearedRawHistory(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	db := testDB(t)
	store := NewUsageStore(db)
	recordIterationPricingFixture(t, store, day)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket([]byte(BucketUsageEvents))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := day, day.Add(time.Hour)
	got, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", missingAdvisorTable())
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.IncompleteCostCount != 1 {
		t.Fatalf("aggregate incomplete=%d after raw retention, want 1", got.Totals.IncompleteCostCount)
	}
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, missingAdvisorTable(), false)
	if err != nil {
		t.Fatal(err)
	}
	var incomplete int64
	for _, unit := range units {
		incomplete += unit.IncompleteCostCount
	}
	if incomplete != 1 {
		t.Fatalf("read cost units incomplete=%d after raw retention, want 1", incomplete)
	}
	summary, err := store.Summary(day.Add(30*time.Minute), missingAdvisorTable())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Today.IncompleteCostCount != 1 {
		t.Fatalf("today incomplete=%d after raw retention, want 1", summary.Today.IncompleteCostCount)
	}
}

func TestSummaryMarksMissingStableIterationPrice(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	store := NewUsageStore(testDB(t))
	recordIterationPricingFixture(t, store, day)
	got, err := store.Summary(day.Add(30*time.Minute), missingAdvisorTable())
	if err != nil {
		t.Fatal(err)
	}
	if got.LastHour.IncompleteCostCount != 1 || got.LastHour.EstimatedCost != 2 {
		t.Fatalf("last_hour cost=%.2f incomplete=%d, want cost=2 incomplete=1", got.LastHour.EstimatedCost, got.LastHour.IncompleteCostCount)
	}
	if got.Today.IncompleteCostCount != 1 || got.Today.EstimatedCost != 2 {
		t.Fatalf("today cost=%.2f incomplete=%d, want cost=2 incomplete=1", got.Today.EstimatedCost, got.Today.IncompleteCostCount)
	}
}

func TestReadCostUnitsMarksMissingStableIterationPrice(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	store := NewUsageStore(testDB(t))
	recordIterationPricingFixture(t, store, day)
	from, to := day.Truncate(time.Hour), day.Truncate(time.Hour).Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, missingAdvisorTable(), false)
	if err != nil {
		t.Fatal(err)
	}
	var advisor, root *CostUnit
	for i := range units {
		switch units[i].Model {
		case "advisor":
			advisor = &units[i]
		case "root":
			root = &units[i]
		}
	}
	if advisor == nil || root == nil {
		t.Fatalf("units=%+v", units)
	}
	if advisor.CostUSD != 0 || advisor.IncompleteCostCount != 1 {
		t.Fatalf("advisor cost=%.2f incomplete=%d, want cost=0 incomplete=1", advisor.CostUSD, advisor.IncompleteCostCount)
	}
	if root.CostUSD != 2 || root.IncompleteCostCount != 0 {
		t.Fatalf("root cost=%.2f incomplete=%d, want cost=2 incomplete=0", root.CostUSD, root.IncompleteCostCount)
	}
}

func TestAggregateModelGroupsMarksMissingStableIterationPrice(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	db := testDB(t)
	store := NewUsageStore(db)
	recordIterationPricingFixture(t, store, day)
	targetStore := NewTargetStore(db)
	if err := targetStore.Create(config.Target{Name: "upstream", Endpoint: "https://example.com", APIKey: "key", UsageAdapter: config.UsageAdapterStandard}); err != nil {
		t.Fatal(err)
	}
	groupStore := NewModelGroupStore(db)
	if err := groupStore.Create(config.ModelGroup{Name: "group-a", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: "upstream"}}}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Truncate(time.Hour), day.Truncate(time.Hour).Add(time.Hour)
	got, err := store.AggregateModelGroups(usage.Filter{From: &from, To: &to}, missingAdvisorTable(), targetStore, groupStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].Totals.IncompleteCostCount != 1 || got.Groups[0].Totals.EstimatedCost != 2 {
		t.Fatalf("groups=%+v, want one group cost=2 incomplete=1", got.Groups)
	}
}

func TestStableAggregateLegacyCellMissingPriceIsConservative(t *testing.T) {
	day := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	from, to := day, day.Add(time.Hour)

	t.Run("legacy billable cell", func(t *testing.T) {
		db := testDB(t)
		if err := db.Update(func(tx *bolt.Tx) error {
			data, err := json.Marshal(AggCell{Requests: 1, InputTokens: 2_000_000})
			if err != nil {
				return err
			}
			return tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey(day, "", "alice", "advisor", "upstream"), data)
		}); err != nil {
			t.Fatal(err)
		}
		got, err := NewUsageStore(db).Aggregate(usage.Filter{From: &from, To: &to}, "hour", usage.CostTable{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Totals.IncompleteCostCount != 1 || got.Totals.EstimatedCost != 0 {
			t.Fatalf("legacy cost=%.2f incomplete=%d, want cost=0 incomplete=1", got.Totals.EstimatedCost, got.Totals.IncompleteCostCount)
		}
	})

	t.Run("legacy non-billable cell", func(t *testing.T) {
		db := testDB(t)
		if err := db.Update(func(tx *bolt.Tx) error {
			data, err := json.Marshal(AggCell{Requests: 1, InputTokens: 1_000_000, NonBillable: &NonBillableTokens{InputTokens: 1_000_000}})
			if err != nil {
				return err
			}
			return tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey(day, "", "alice", "free", "upstream"), data)
		}); err != nil {
			t.Fatal(err)
		}
		got, err := NewUsageStore(db).Aggregate(usage.Filter{From: &from, To: &to}, "hour", usage.CostTable{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Totals.IncompleteCostCount != 0 {
			t.Fatalf("legacy non-billable incomplete=%d, want 0", got.Totals.IncompleteCostCount)
		}
	})

	t.Run("legacy quota cell through ReadCostUnits", func(t *testing.T) {
		db := testDB(t)
		if err := db.Update(func(tx *bolt.Tx) error {
			data, err := json.Marshal(AggCell{Requests: 1, InputTokens: 2_000_000})
			if err != nil {
				return err
			}
			return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(quotaAggKey(day, "alice", "upstream", "advisor"), data)
		}); err != nil {
			t.Fatal(err)
		}
		units, err := NewUsageStore(db).ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, usage.CostTable{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(units) != 1 || units[0].IncompleteCostCount != 1 {
			t.Fatalf("legacy units=%+v, want one incomplete unit", units)
		}
	})
}
