package nosql

import (
	"context"
	"encoding/json"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"math"
	"testing"
	"time"
)

func TestImagePermanentRollupAndLegacyValues(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	stamp := time.Now().UTC().Add(-2 * time.Hour)
	var legacy usage.Event
	if err := json.Unmarshal([]byte(`{"client_name":"alice","target":"upstream","model":"image","input_tokens":1000000,"output_tokens":1000000,"cached_tokens":1000000,"status_code":200}`), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Timestamp = stamp
	if legacy.ImageInputTokens != 0 || legacy.CachedImageInputTokens != 0 || legacy.ImageOutputTokens != 0 {
		t.Fatal("legacy reclassified")
	}
	image := legacy
	image.Timestamp = stamp.Add(time.Second)
	image.ImageInputTokens = 200000
	image.CachedImageInputTokens = 300000
	image.ImageOutputTokens = 400000
	for _, evt := range []usage.Event{legacy, image} {
		if err := store.Record(evt); err != nil {
			t.Fatal(err)
		}
	}
	in, cache, out := 10.0, 3.0, 20.0
	rates := usage.CostTable{"image": {InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 1, ImageInputPer1MTokens: &in, CachedImageInputPer1MTokens: &cache, ImageOutputPer1MTokens: &out}}
	if got := usage.EstimateEventCost(legacy, rates); got != 7 {
		t.Fatalf("legacy cost=%v", got)
	}
	from, to := stamp.Truncate(time.Hour), stamp.Add(time.Minute)
	filter := usage.Filter{From: &from, To: &to, ClientName: "alice"}
	check := func() {
		t.Helper()
		units, err := store.ReadCostUnits(context.Background(), filter, rates, true)
		if err != nil || len(units) != 1 || units[0].Requests != 2 || math.Abs(units[0].CostUSD-22.6) > 1e-10 || units[0].ImageInputTokens != 200000 || units[0].CachedImageInputTokens != 300000 || units[0].ImageOutputTokens != 400000 {
			t.Fatalf("rollup=%+v err=%v", units, err)
		}
	}
	check()
	if count, err := store.PruneEventsBefore(stamp.Add(time.Hour), 10); err != nil || count != 2 {
		t.Fatalf("pruned=%d err=%v", count, err)
	}
	check()
	// Persisted old aggregation JSON needs no rebuild or migration.
	var old AggCell
	if err := json.Unmarshal([]byte(`{"requests":1,"input_tokens":1000000,"output_tokens":1000000,"cached_tokens":1000000}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := aggCellCost(old, "upstream", stamp, "image", rates); got != 7 {
		t.Fatalf("legacy cell cost=%v", got)
	}
}

func TestImageOverageDirectRecordNormalized(t *testing.T) {
	store := NewUsageStore(testDB(t))
	stamp := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour).Add(time.Minute)
	for i, images := range []int64{2000000, 0} {
		if err := store.Record(usage.Event{Timestamp: stamp.Add(time.Duration(i) * time.Second), ClientName: "alice", Target: "upstream", Model: "image", InputTokens: 1000000, ImageInputTokens: images, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	image := 10.0
	rates := usage.CostTable{"image": {InputPer1MTokens: 2, ImageInputPer1MTokens: &image}}
	from, to := stamp.Truncate(time.Hour), stamp.Add(time.Minute)
	filter := usage.Filter{From: &from, To: &to, ClientName: "alice"}
	check := func() {
		t.Helper()
		units, err := store.ReadCostUnits(context.Background(), filter, rates, true)
		if err != nil || len(units) != 1 || units[0].CostUSD != 12 || units[0].ImageInputTokens != 1000000 {
			t.Fatalf("units=%+v err=%v", units, err)
		}
	}
	check()
	if n, err := store.PruneEventsBefore(to, 10); err != nil || n != 2 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	check()
}

func TestImageModelCostsCloneAndValidate(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)
	in, free, out := 1.234567, 0.0, 9.876543
	for _, target := range []string{"", "upstream"} {
		if err := store.Upsert(ModelCost{Target: target, Model: "image", ImageInputPer1MTokens: &in, CachedImageInputPer1MTokens: &free, ImageOutputPer1MTokens: &out}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List()
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	for _, cost := range items {
		if cost.ImageInputPer1MTokens == nil || *cost.ImageInputPer1MTokens != in || cost.CachedImageInputPer1MTokens == nil || *cost.CachedImageInputPer1MTokens != 0 || cost.ImageOutputPer1MTokens == nil || *cost.ImageOutputPer1MTokens != out {
			t.Fatalf("prices lost %+v", cost)
		}
	}
	cloned := cloneCosts(items)
	*cloned[0].ImageInputPer1MTokens = 99
	if *items[0].ImageInputPer1MTokens != in {
		t.Fatal("price pointer aliased")
	}
	negative := -1.0
	if err := store.Upsert(ModelCost{Model: "image", ImageInputPer1MTokens: &negative}); err == nil {
		t.Fatal("accepted negative image price")
	}
}
