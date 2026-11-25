package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// 展示读取路径（Issue：几百人规模下报表等待十几秒）：对不需要事件时间戳的
// 定价，Summary / Aggregate / ModelGroups 只读小时汇总，不再对窗口内每条
// raw 事件做完整性游走。计费、额度、导出继续使用严格读取。

type displayFixture struct {
	db    *bolt.DB
	store *UsageStore
	now   time.Time
	from  time.Time
}

func newDisplayFixture(t *testing.T) displayFixture {
	t.Helper()
	db := testDB(t)
	store := NewUsageStore(db)
	now := time.Now().UTC().Truncate(time.Hour).Add(20 * time.Minute)
	from := now.Add(-10 * 24 * time.Hour)
	var events []usage.Event
	for i := 0; i < 900; i++ {
		// 横跨 10 天；最近一小时内也有事件，覆盖 last_hour 的 raw 精确窗口。
		ts := from.Add(time.Duration(i) * 16 * time.Minute)
		if i >= 880 {
			ts = now.Add(-time.Duration(900-i) * 2 * time.Minute)
		}
		status := 200
		if i%11 == 0 {
			status = 500
		}
		events = append(events, usage.Event{
			Timestamp: ts, ClientName: fmt.Sprintf("client-%02d", i%7), EndpointType: "openai",
			Model: []string{"qwen-max", "glm-max", "deepseek-chat"}[i%3], Target: fmt.Sprintf("t%d", i%4),
			InputTokens: int64(1000 + i), OutputTokens: int64(100 + i%50), CachedTokens: int64(i % 40), StatusCode: status,
			RequestID: fmt.Sprintf("req-%d", i), RecordID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Path: "/v1/chat/completions",
		})
	}
	for start := 0; start < len(events); start += 128 {
		end := min(start+128, len(events))
		if err := store.RecordBatch(events[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	return displayFixture{db: db, store: store, now: now, from: from}
}

func stableResolver() usage.RateResolver {
	return pricing.NewResolver([]pricing.PricingTemplate{
		{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 6}},
		{Model: "deepseek-chat", Mode: pricing.ModeSchedule, Timezone: "+08:00", BaseRates: usage.CostRates{InputPer1MTokens: 1},
			Periods: []pricing.PricingPeriod{{Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: "09:00", End: "17:00", Rates: usage.CostRates{InputPer1MTokens: 3}}}},
	}, nil)
}

// 带外删除一条事件对应的 quota 小时汇总：raw 事件仍在，汇总缺失。严格读取按
// "raw 无对应汇总"拒绝；展示读取不读 raw，照常返回（该事件被漏计，由后台校验标红）。
func deleteRollupOfEvent(t *testing.T, fx displayFixture, i int) {
	t.Helper()
	ts := fx.from.Add(time.Duration(i) * 16 * time.Minute)
	key := quotaAggKey(ts, fmt.Sprintf("client-%02d", i%7), fmt.Sprintf("t%d", i%4), []string{"qwen-max", "glm-max", "deepseek-chat"}[i%3])
	if err := fx.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		if b.Get(key) == nil {
			return fmt.Errorf("rollup %q not found", key)
		}
		return b.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDisplayReportsMatchStrictOnHealthyData(t *testing.T) {
	fx := newDisplayFixture(t)
	r := stableResolver()
	if usage.RequiresEventTimestamps(r) {
		t.Fatal("fixture resolver must be hour-aligned")
	}
	ctx := context.Background()

	t.Run("summary", func(t *testing.T) {
		want, err := fx.store.SummaryContext(ctx, fx.now, r)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fx.store.SummaryDisplayContext(ctx, fx.now, r)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("summary mismatch:\n got=%+v\nwant=%+v", got, want)
		}
		if want.LastHour.Requests == 0 || want.Last30Days.Requests == 0 {
			t.Fatalf("fixture must populate last hour and 30d: %+v", want)
		}
	})

	filters := map[string]usage.Filter{
		"all":         {From: &fx.from, To: &fx.now},
		"open-ended":  {From: &fx.from},
		"one-client":  {From: &fx.from, To: &fx.now, ClientName: "client-03"},
		"one-model":   {From: &fx.from, To: &fx.now, Model: "qwen-max"},
		"mid-hour":    {From: ptrTime(fx.from.Add(17 * time.Minute)), To: ptrTime(fx.now.Add(-37 * time.Minute))},
		"on-the-hour": {From: ptrTime(fx.now.Truncate(time.Hour).Add(-48 * time.Hour)), To: ptrTime(fx.now.Truncate(time.Hour))},
	}
	for name, filter := range filters {
		for _, groupBy := range []string{"hour", "day"} {
			t.Run("aggregate/"+name+"/"+groupBy, func(t *testing.T) {
				want, err := fx.store.AggregateByModelGroupsAllRequestsContext(ctx, filter, groupBy, r, nil)
				if err != nil {
					t.Fatal(err)
				}
				got, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, groupBy, r, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("aggregate mismatch:\n got=%+v\nwant=%+v", got, want)
				}
			})
		}
		t.Run("success-requests/"+name, func(t *testing.T) {
			want, err := fx.store.AggregateByModelGroupsContext(ctx, filter, "day", r, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fx.store.AggregateByModelGroupsDisplayContext(ctx, filter, "day", r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("success aggregate mismatch:\n got=%+v\nwant=%+v", got, want)
			}
		})
		t.Run("model-groups/"+name, func(t *testing.T) {
			want, err := fx.store.AggregateModelGroupsContext(ctx, filter, r, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fx.store.AggregateModelGroupsDisplayContext(ctx, filter, r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("model groups mismatch:\n got=%+v\nwant=%+v", got, want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestDisplayReportsDoNotWalkRawEvents(t *testing.T) {
	fx := newDisplayFixture(t)
	r := stableResolver()
	ctx := context.Background()

	count := func(hooks *costReadHooks, run func(costReadHooks) error) (raw int) {
		hooks.direct = &directIntegrityHooks{raw: func() { raw++ }, call: func(time.Time, time.Time) {}}
		if err := run(*hooks); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	filter := usage.Filter{From: &fx.from, To: &fx.now}

	strict := count(&costReadHooks{}, func(h costReadHooks) error {
		_, err := fx.store.aggregateTargetModelCellsReadContext(ctx, filter, "day", r, nil, false, h)
		return err
	})
	if strict == 0 {
		t.Fatal("strict read is expected to walk raw events; the comparison below would prove nothing")
	}
	display := count(&costReadHooks{display: true}, func(h costReadHooks) error {
		_, err := fx.store.aggregateTargetModelCellsReadContext(ctx, filter, "day", r, nil, false, h)
		return err
	})
	if display != 0 {
		t.Fatalf("display aggregate walked %d raw events", display)
	}
	decoded := 0
	count(&costReadHooks{display: true, decoded: func() { decoded++ }}, func(h costReadHooks) error {
		_, err := fx.store.summaryReadContext(ctx, fx.now, r, h)
		return err
	})
	// last_hour 需要精确到分钟，只允许读取最近约一小时的 raw 行（≈ 30 条），
	// 绝不是 10 天全量（900 条）。
	if decoded == 0 || decoded > 60 {
		t.Fatalf("display summary decoded %d raw rows, want only the last hour", decoded)
	}
}

func TestDisplayReportsSurviveRawIntegrityDamageButStrictStillFails(t *testing.T) {
	fx := newDisplayFixture(t)
	r := stableResolver()
	ctx := context.Background()
	deleteRollupOfEvent(t, fx, 300)
	filter := usage.Filter{From: &fx.from, To: &fx.now}

	if _, err := fx.store.AggregateByModelGroupsAllRequestsContext(ctx, filter, "day", r, nil); err == nil {
		t.Fatal("strict aggregate must keep failing closed on missing rollup (billing-grade semantics unchanged)")
	}
	if _, err := fx.store.SummaryContext(ctx, fx.now, r); err == nil {
		t.Fatal("strict summary must keep failing closed on missing rollup")
	}
	if _, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil); err != nil {
		t.Fatalf("display aggregate must not be blocked by raw integrity damage: %v", err)
	}
	if _, err := fx.store.SummaryDisplayContext(ctx, fx.now, r); err != nil {
		t.Fatalf("display summary must not be blocked by raw integrity damage: %v", err)
	}
	if _, err := fx.store.AggregateModelGroupsDisplayContext(ctx, filter, r, nil); err != nil {
		t.Fatalf("display model groups must not be blocked: %v", err)
	}
}

// 损坏的历史数据块（无法解码的汇总单元、畸形键）直接丢弃：展示读取跳过并告警，
// 其余数据照常统计，不再让整页 500（与 Count 的既有做法一致）。汇总来源整体缺失
// 仍然报错，因为那意味着不知道缺了多少。
func TestDisplayReportsDiscardCorruptRollupCells(t *testing.T) {
	fx := newDisplayFixture(t)
	r := stableResolver()
	ctx := context.Background()
	filter := usage.Filter{From: &fx.from, To: &fx.now}

	healthy, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	var victim []byte
	var victimRequests int64
	if err := fx.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		k, v := b.Cursor().Seek(hourPrefix(fx.from.Add(24 * time.Hour)))
		victim = append([]byte(nil), k...)
		var cell AggCell
		if err := json.Unmarshal(v, &cell); err != nil {
			return err
		}
		victimRequests = cell.Requests
		if err := b.Put(victim, []byte("{not json")); err != nil {
			return err
		}
		// 畸形键：无法解析出完整维度。
		return b.Put([]byte(fx.from.Add(48*time.Hour).Format(hourLayout)+"|broken"), []byte("{}"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.AggregateByModelGroupsAllRequestsContext(ctx, filter, "day", r, nil); err == nil {
		t.Fatal("strict read must keep failing on a corrupt rollup")
	}
	got, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil)
	if err != nil {
		t.Fatalf("display read must discard corrupt blocks instead of failing: %v", err)
	}
	if want := healthy.Totals.Requests - victimRequests; got.Totals.Requests != want {
		t.Fatalf("requests=%d, want %d (healthy %d minus discarded cell %d)", got.Totals.Requests, want, healthy.Totals.Requests, victimRequests)
	}
	if _, err := fx.store.SummaryDisplayContext(ctx, fx.now, r); err != nil {
		t.Fatalf("display summary must discard corrupt blocks: %v", err)
	}
}

func TestDisplayReportsStillRejectMissingRollupSource(t *testing.T) {
	fx := newDisplayFixture(t)
	if err := fx.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageAggregateSourceMissingPrefix+BucketQuotaUsageAggHourly), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.SummaryDisplayContext(context.Background(), fx.now, stableResolver()); err == nil {
		t.Fatal("missing quota rollup source must still fail display reads")
	}
}

func TestDisplayReportsFallBackToStrictForEventTimestampPricing(t *testing.T) {
	fx := newDisplayFixture(t)
	// 非整点 Start：需要事件时间戳，展示路径必须回退到严格读取，结果不变且仍 fail-closed。
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: fx.from.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil)
	if !usage.RequiresEventTimestamps(r) {
		t.Fatal("fixture resolver must require event timestamps")
	}
	ctx := context.Background()
	filter := usage.Filter{From: &fx.from, To: &fx.now}
	want, err := fx.store.AggregateByModelGroupsAllRequestsContext(ctx, filter, "day", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback result differs:\n got=%+v\nwant=%+v", got, want)
	}
	deleteRollupOfEvent(t, fx, 300)
	if _, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil); err == nil {
		t.Fatal("event-timestamp pricing must keep failing closed on missing rollup")
	}
}

func TestDisplayReportsHonorCancellation(t *testing.T) {
	fx := newDisplayFixture(t)
	r := stableResolver()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	filter := usage.Filter{From: &fx.from, To: &fx.now}
	if _, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("aggregate: %v", err)
	}
	if _, err := fx.store.SummaryDisplayContext(ctx, fx.now, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("summary: %v", err)
	}
}

// 生产环境的解析器外面套了系数包装；+00:00 模板属于整点对齐。两者下展示读取
// 必须与严格读取逐字段相等，且确实走小时汇总路径（不读 raw）。
func TestDisplayReportsMatchStrictWithCoefficientAndUTCTemplate(t *testing.T) {
	fx := newDisplayFixture(t)
	inner := pricing.NewResolver([]pricing.PricingTemplate{
		{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 6}},
		{Model: "deepseek-chat", Start: fx.from.Truncate(time.Hour), Mode: pricing.ModeSchedule, Timezone: "+00:00",
			BaseRates: usage.CostRates{InputPer1MTokens: 1},
			Periods:   []pricing.PricingPeriod{{Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: "09:00", End: "17:00", Rates: usage.CostRates{InputPer1MTokens: 3}}}},
	}, nil)
	r := usage.WithCoefficient(inner, func(target, model string) float64 {
		if model == "glm-max" {
			return 0 // 免费折算
		}
		return 0.5
	})
	if usage.RequiresEventTimestamps(r) {
		t.Fatal("coefficient wrapper must preserve hour alignment")
	}
	ctx := context.Background()
	filter := usage.Filter{From: &fx.from, To: &fx.now}

	wantSummary, err := fx.store.SummaryContext(ctx, fx.now, r)
	if err != nil {
		t.Fatal(err)
	}
	gotSummary, err := fx.store.SummaryDisplayContext(ctx, fx.now, r)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotSummary, wantSummary) {
		t.Fatalf("summary mismatch:\n got=%+v\nwant=%+v", gotSummary, wantSummary)
	}
	want, err := fx.store.AggregateByModelGroupsAllRequestsContext(ctx, filter, "day", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fx.store.AggregateByModelGroupsAllRequestsDisplayContext(ctx, filter, "day", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregate mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if want.Totals.EstimatedCost <= 0 {
		t.Fatalf("fixture must produce a priced total: %+v", want.Totals)
	}
	raw := 0
	hooks := costReadHooks{display: true, direct: &directIntegrityHooks{raw: func() { raw++ }, call: func(time.Time, time.Time) {}}}
	if _, err := fx.store.aggregateTargetModelCellsReadContext(ctx, filter, "day", r, nil, false, hooks); err != nil {
		t.Fatal(err)
	}
	if raw != 0 {
		t.Fatalf("display path walked %d raw events under coefficient wrapper", raw)
	}
}

// 小时汇总键始终按 UTC 切分，不受事件自带时区（UTC+8 部署）影响。
func TestQuotaAggKeyUsesUTCHour(t *testing.T) {
	cst := time.FixedZone("UTC+8", 8*3600)
	at := time.Date(2026, 9, 7, 7, 30, 0, 0, cst) // = 2026-09-06T23:30Z
	if got := string(quotaAggKey(at, "alice", "t", "m")); !strings.HasPrefix(got, "2026-09-06T23|") {
		t.Fatalf("quota key %q must start with the UTC hour 2026-09-06T23", got)
	}
	if got := string(aggKey(at, "openai", "alice", "m", "t")); !strings.Contains(got, "2026-09-06T23") {
		t.Fatalf("agg key %q must use the UTC hour", got)
	}
}
