package nosql

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

type quotaWindowRates struct{ exact bool }

func (r quotaWindowRates) RateFor(string, string, time.Time) (usage.CostRates, bool) {
	return usage.CostRates{InputPer1MTokens: 3, OutputPer1MTokens: 7}, true
}
func (r quotaWindowRates) RequiresEventTimestamps() bool { return r.exact }

func sortedUnits(u []CostUnit) []CostUnit {
	out := append([]CostUnit(nil), u...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return fmt.Sprint(a.Client, a.Target, a.Model) < fmt.Sprint(b.Client, b.Target, b.Model)
	})
	return out
}

// 额度窗口读取与全量校验读取在多窗口、多目标/模型、跨周期数据上逐项一致。
func TestReadQuotaCostUnitsMatchesFullValidation(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Now().UTC().Truncate(time.Minute)
	for i := 0; i < 300; i++ {
		e := usage.Event{
			Timestamp:  now.Add(-time.Duration(i*397) * time.Minute),
			ClientName: []string{"alice", "bob"}[i%2], Target: fmt.Sprintf("t%d", i%3), Model: []string{"qwen-max", "glm-max"}[i%2],
			InputTokens: int64(1000 + i), OutputTokens: int64(10 + i%7), StatusCode: 200,
		}
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	total := 0
	for _, exact := range []bool{false, true} {
		for _, span := range []time.Duration{3 * time.Hour, 26 * time.Hour, 9 * 24 * time.Hour, 45 * 24 * time.Hour} {
			from, to := now.Add(-span), now
			for _, client := range []string{"alice", "bob"} {
				f := usage.Filter{ClientName: client, From: &from, To: &to}
				want, err1 := store.ReadCostUnits(ctx, f, quotaWindowRates{exact}, true)
				got, err2 := store.ReadQuotaCostUnits(ctx, f, quotaWindowRates{exact}, true)
				if err1 != nil || err2 != nil {
					t.Fatalf("exact=%v span=%v client=%s errs: %v / %v", exact, span, client, err1, err2)
				}
				total += len(want)
				if !reflect.DeepEqual(sortedUnits(want), sortedUnits(got)) {
					t.Fatalf("exact=%v span=%v client=%s units differ:\nwant=%v\ngot=%v", exact, span, client, want, got)
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("fixture windows produced no units")
	}
}

// 窗口外聚合键数量放大 100 倍，窗口读取耗时不应随之线性增长。
func TestReadQuotaCostUnitsDoesNotScaleWithOutOfWindowBuckets(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 20; i++ {
		e := usage.Event{Timestamp: now.Add(-time.Duration(i) * time.Hour), ClientName: "alice", Target: "t", Model: "m", InputTokens: 1000, StatusCode: 200}
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	from, to := now.Add(-48*time.Hour), now.Add(time.Hour)
	f := usage.Filter{ClientName: "alice", From: &from, To: &to}
	measure := func(read func(context.Context, usage.Filter, usage.RateResolver, bool) ([]CostUnit, error)) time.Duration {
		best := time.Hour
		for i := 0; i < 5; i++ {
			st := time.Now()
			if _, err := read(context.Background(), f, quotaWindowRates{}, true); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(st); d < best {
				best = d
			}
		}
		return best
	}
	fill := func(n, base int) {
		if err := db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
			for i := 0; i < n; i++ {
				at := now.Add(-time.Duration(90*24+base+i) * time.Hour)
				if err := b.Put(quotaAggKey(at, "other", "t", "m"), []byte(`{"requests":1,"record_count":1}`)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	fill(500, 0)
	small := measure(store.ReadQuotaCostUnits)
	fill(100000, 500)
	large := measure(store.ReadQuotaCostUnits)
	full := measure(store.ReadCostUnits)
	t.Logf("windowed small=%v large=%v ; full-validation large=%v", small, large, full)
	if large > 5*small+5*time.Millisecond {
		t.Fatalf("windowed read scales with out-of-window buckets: small=%v large=%v", small, large)
	}
}
