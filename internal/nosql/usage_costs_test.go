package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

func TestExactTargetModelCostsMinuteTimezoneAndCoverage(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		templates []pricing.PricingTemplate
		events    []usage.Event
		want      float64
	}{
		{"minute period", []pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00",
			BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}},
			[]usage.Event{{Timestamp: day.Add(9*time.Hour + 10*time.Minute), InputTokens: 1_000_000}, {Timestamp: day.Add(9*time.Hour + 45*time.Minute), InputTokens: 2_000_000}}, 21},
		{"half-hour timezone", []pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+05:30",
			BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:00", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}},
			[]usage.Event{{Timestamp: day.Add(3*time.Hour + 10*time.Minute), InputTokens: 1_000_000}, {Timestamp: day.Add(3*time.Hour + 45*time.Minute), InputTokens: 2_000_000}}, 21},
		{"coverage inside hour", []pricing.PricingTemplate{{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}},
			{Model: "qwen-max", Start: day.Add(9*time.Hour + 30*time.Minute), End: timePointer(day.Add(9*time.Hour + 50*time.Minute)), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 10}}},
			[]usage.Event{{Timestamp: day.Add(9*time.Hour + 10*time.Minute), InputTokens: 1_000_000}, {Timestamp: day.Add(9*time.Hour + 45*time.Minute), InputTokens: 2_000_000}, {Timestamp: day.Add(9*time.Hour + 55*time.Minute), InputTokens: 3_000_000}}, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			for _, event := range tc.events {
				event.ClientName, event.Target, event.Model, event.StatusCode = "alice", "upstream", "qwen-max", 200
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			to := day.Add(24 * time.Hour)
			cells, err := store.SumTargetModelCostsRange(usage.Filter{From: &day, To: &to, ClientName: "alice"}, pricing.NewResolver(tc.templates, nil))
			if err != nil || len(cells) != 1 || cells[0].CostUSD != tc.want {
				t.Fatalf("exact cost=%+v err=%v, want %v", cells, err, tc.want)
			}
		})
	}
}

func timePointer(at time.Time) *time.Time { return &at }

func TestExactTargetModelCostsStreamBeyondListLimitAndRejectMissingHistory(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	const count = 2501
	var firstKey []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		for i := 0; i < count; i++ {
			event := usage.Event{Timestamp: day.Add(45*time.Minute + time.Duration(i)*time.Millisecond), RecordID: uuid.NewString(),
				ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200}
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			key := []byte(usageKey(event.Timestamp, event.RecordID))
			if i == 0 {
				firstKey = key
			}
			if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, data); err != nil {
				return err
			}
			if err := bumpQuotaAgg(tx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resolver := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat,
		BaseRates: usage.CostRates{InputPer1MTokens: 10}}}, nil)
	to := day.Add(time.Hour)
	filter := usage.Filter{From: &day, To: &to, ClientName: "alice", Limit: 1}
	cells, err := store.SumTargetModelCostsRange(filter, resolver)
	if err != nil || len(cells) != 1 || cells[0].CostUSD != count*10 {
		t.Fatalf("streamed costs=%+v err=%v, want %d events", cells, err, count)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Delete(firstKey) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SumTargetModelCostsRange(filter, resolver); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing raw event must reject guessed cost: %v", err)
	}
}

func TestHourlyTargetModelCostsDoNotRequirePrunedRawHistory(t *testing.T) {
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if err := db.Update(func(tx *bolt.Tx) error {
		cell := AggCell{Requests: 3000, InputTokens: 1_000_000}
		data, err := json.Marshal(cell)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put([]byte(fmt.Sprintf("%s|alice|upstream|qwen-max", day.Format(hourLayout))), data)
	}); err != nil {
		t.Fatal(err)
	}
	to := day.Add(time.Hour)
	resolver := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeFlat,
		BaseRates: usage.CostRates{InputPer1MTokens: 2}},
		// An unrelated key needs precise pricing, but must not make this
		// client's surviving hourly-only history unusable.
		{Model: "glm-flash", Start: day.Add(time.Minute), Mode: pricing.ModeFlat}}, nil)
	cells, err := NewUsageStore(db).SumTargetModelCostsRange(usage.Filter{From: &day, To: &to, ClientName: "alice"}, resolver)
	if err != nil || len(cells) != 1 || cells[0].CostUSD != 2 {
		t.Fatalf("hourly fast path costs=%+v err=%v", cells, err)
	}
}

func TestExactTargetModelCostsIsolateCorruptEventToItsClient(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	resolver := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00",
		BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}}, nil)
	db := testDB(t)
	store := NewUsageStore(db)
	for _, client := range []string{"alice", "bob"} {
		if err := store.Record(usage.Event{Timestamp: day.Add(9*time.Hour + 45*time.Minute), ClientName: client, Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt bob's raw event in place: its owner can no longer be decoded.
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketUsageEvents))
		return bucket.ForEach(func(k, v []byte) error {
			var event usage.Event
			if json.Unmarshal(v, &event) == nil && event.ClientName == "bob" {
				return bucket.Put(append([]byte(nil), k...), []byte("{corrupt"))
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	to := day.Add(24 * time.Hour)
	cells, err := store.SumTargetModelCostsRange(usage.Filter{From: &day, To: &to, ClientName: "alice"}, resolver)
	if err != nil || len(cells) != 1 || cells[0].CostUSD != 10 {
		t.Fatalf("another client's corrupt event blocked calibration: cells=%+v err=%v", cells, err)
	}
	// bob's own rollup still expects the event, so his history stays incomplete.
	if _, err := store.SumTargetModelCostsRange(usage.Filter{From: &day, To: &to, ClientName: "bob"}, resolver); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("corrupt own event was not detected: %v", err)
	}
}
