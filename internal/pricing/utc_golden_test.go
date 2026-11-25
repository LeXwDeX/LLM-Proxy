package pricing

import (
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// 冻结 UTC 口径：时段按模板固定偏移解释，边界都在 UTC 整点；
// 默认时区为 +08:00。改默认时区或对齐判定，这里会立刻失败。

func goldenResolver(tz string, periods []PricingPeriod) *Resolver {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return NewResolver([]PricingTemplate{{
		Model: "m", Start: start, Mode: ModeSchedule, Timezone: tz,
		BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: periods,
	}}, nil)
}

func inputRate(t *testing.T, r *Resolver, at string) float64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatal(err)
	}
	rates, ok := r.RateFor("", "m", ts)
	if !ok {
		t.Fatalf("no rate at %s", at)
	}
	return rates.InputPer1MTokens
}

func TestUTCGoldenBoundaries(t *testing.T) {
	if DefaultTimezone != "+08:00" {
		t.Fatalf("DefaultTimezone=%q; changing it silently shifts stored schedules that omit timezone", DefaultTimezone)
	}
	monday := []PricingPeriod{{Weekdays: []int{1}, Start: "09:00", End: "17:00", Rates: usage.CostRates{InputPer1MTokens: 3}}}
	// 2026-09-07 是周一。空时区 → +08:00：本地 09:00–17:00 即 01:00Z–09:00Z。
	for _, tc := range []struct {
		tz, at string
		want   float64
	}{
		{"", "2026-09-07T00:59:59Z", 1},
		{"", "2026-09-07T01:00:00Z", 3},
		{"", "2026-09-07T08:59:59Z", 3},
		{"", "2026-09-07T09:00:00Z", 1},
		{"+08:00", "2026-09-07T01:00:00Z", 3},
		{"+00:00", "2026-09-07T08:59:59Z", 1},
		{"+00:00", "2026-09-07T09:00:00Z", 3},
		{"+00:00", "2026-09-07T16:59:59Z", 3},
		{"+00:00", "2026-09-07T17:00:00Z", 1},
	} {
		if got := inputRate(t, goldenResolver(tc.tz, monday), tc.at); got != tc.want {
			t.Errorf("tz=%q at %s: rate=%v want %v", tc.tz, tc.at, got, tc.want)
		}
	}
	// 周与 24:00：+08:00 周一整天 = 周日 16:00Z 起至周一 16:00Z（不含）。
	day := []PricingPeriod{{Weekdays: []int{1}, Start: "00:00", End: "24:00", Rates: usage.CostRates{InputPer1MTokens: 3}}}
	r := goldenResolver("+08:00", day)
	for at, want := range map[string]float64{
		"2026-09-06T15:59:59Z": 1,
		"2026-09-06T16:00:00Z": 3,
		"2026-09-07T15:59:59Z": 3,
		"2026-09-07T16:00:00Z": 1,
	} {
		if got := inputRate(t, r, at); got != want {
			t.Errorf("weekday edge at %s: rate=%v want %v", at, got, want)
		}
	}
}

// 判定为"整点对齐"的模板，在同一 UTC 小时内任意时刻选价相同；
// 因此小时汇总可以代替事件时间戳。非整点模板必须被判定为需要事件时间戳。
func TestUTCAlignedTemplatesPriceIdenticallyWithinEachHour(t *testing.T) {
	periods := []PricingPeriod{{Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Start: "09:00", End: "17:00", Rates: usage.CostRates{InputPer1MTokens: 3}}}
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	for _, tz := range []string{"", "+00:00", "+08:00", "UTC"} {
		r := goldenResolver(tz, periods)
		if r.RequiresEventTimestamps() {
			t.Fatalf("tz=%q must be hour-aligned", tz)
		}
		for h := 0; h < 72; h++ {
			hour := base.Add(time.Duration(h) * time.Hour)
			want, _ := r.RateFor("", "m", hour)
			for _, off := range []time.Duration{30 * time.Minute, 59*time.Minute + 59*time.Second + 999999999} {
				if got, _ := r.RateFor("", "m", hour.Add(off)); got != want {
					t.Fatalf("tz=%q hour %s: rate changes inside the hour (%v vs %v)", tz, hour, got, want)
				}
			}
		}
	}
	// 半小时偏移：必须要求事件时间戳，且确有小时内变价。
	half := goldenResolver("+05:30", periods)
	if !half.RequiresEventTimestamps() {
		t.Fatal("+05:30 must require event timestamps")
	}
	changed := false
	for h := 0; h < 48 && !changed; h++ {
		hour := base.Add(time.Duration(h) * time.Hour)
		a, _ := half.RateFor("", "m", hour)
		b, _ := half.RateFor("", "m", hour.Add(59*time.Minute))
		changed = a != b
	}
	if !changed {
		t.Fatal("expected a mid-hour price change for +05:30")
	}
}
