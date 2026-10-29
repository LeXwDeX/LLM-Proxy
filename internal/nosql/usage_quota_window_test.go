package nosql

import (
	"context"
	"encoding/json"
	"errors"
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

// 额度键格式全量校验：窗口外「小时无法解析」的畸形键也必须使额度读取 fail-closed。
func TestReadQuotaCostUnitsRejectsMalformedKeyOutsideWindow(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Now().UTC().Truncate(time.Minute)
	if err := store.Record(usage.Event{Timestamp: now, ClientName: "alice", Target: "t", Model: "m", InputTokens: 1000, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put([]byte("zz-bad-hour|alice|t|m"), []byte(`{"requests":1,"record_count":1}`))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := now.Add(-48*time.Hour), now
	f := usage.Filter{ClientName: "alice", From: &from, To: &to}
	if units, err := store.ReadQuotaCostUnits(context.Background(), f, quotaWindowRates{}, true); !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
		t.Fatalf("quota path must fail closed on out-of-window malformed key: units=%v err=%v", units, err)
	}
	if _, err := store.ReadCostUnits(context.Background(), f, quotaWindowRates{}, true); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("report path must also fail: %v", err)
	}
}

// 报表路径（带 allow）：空 target 与 "unknown" 共享 usage 聚合键。范围外（被 allow
// 排除）的空 target 事件仍须参与对照，否则对共享聚合格的篡改无法发现。
func TestDirectIntegrityAllowPathKeepsOutOfScopeEventsForSharedUsageKey(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Now().UTC().Truncate(time.Hour).Add(5 * time.Minute)
	for _, target := range []string{"", "unknown"} {
		if err := store.Record(usage.Event{Timestamp: now, ClientName: "alice", Target: target, Model: "m", InputTokens: 1000, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	// 把共享的 usage 聚合格篡改为只含 "unknown" 那一条事件。
	if err := db.Update(func(tx *bolt.Tx) error {
		agg := tx.Bucket([]byte(BucketUsageAggHourly))
		key := aggKey(now, "", "alice", "m", "")
		if agg.Get(key) == nil {
			return fmt.Errorf("shared usage cell missing")
		}
		var cell AggCell
		accumulateAggCell(&cell, usage.Event{Timestamp: now, ClientName: "alice", Target: "unknown", Model: "m", InputTokens: 1000, StatusCode: 200})
		data, err := json.Marshal(cell)
		if err != nil {
			return err
		}
		return agg.Put(key, data)
	}); err != nil {
		t.Fatal(err)
	}
	from, to := now.Truncate(time.Hour), now.Truncate(time.Hour).Add(time.Hour)
	f := usage.Filter{ClientName: "alice", From: &from, To: &to}
	allow := costPairFilter(func(target, model string) bool { return target == "unknown" })
	for name, hooks := range map[string]*directIntegrityHooks{"report": nil, "report+windowKeys": {windowKeys: true}} {
		err := db.View(func(tx *bolt.Tx) error { return checkDirectQuotaEvidence(tx, f, from, to, allow, hooks) })
		if !errors.Is(err, ErrUsageHistoryIncomplete) {
			t.Fatalf("%s: tampered shared usage cell undetected: %v", name, err)
		}
	}
}
