package pricing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/catalog"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// r builds a CostRates with the four token prices.
func r(in, out, cw, cr float64) usage.CostRates {
	return usage.CostRates{
		InputPer1MTokens:     in,
		OutputPer1MTokens:    out,
		CacheWritePer1MToken: cw,
		CacheReadPer1MToken:  cr,
	}
}

// tp returns a pointer to t.
func tp(t time.Time) *time.Time { return &t }

func TestIsTemplate0(t *testing.T) {
	zero := PricingTemplate{}
	if !IsTemplate0(&zero) {
		t.Fatal("zero Start must be template 0")
	}
	nonZero := PricingTemplate{Start: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	if IsTemplate0(&nonZero) {
		t.Fatal("non-zero Start must not be template 0")
	}
}

func TestRateFor_Template0Fallback(t *testing.T) {
	base := r(1, 2, 3, 4)
	templates := []PricingTemplate{
		{
			ID: "t0", Target: "", Model: "m",
			Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: base,
		},
	}
	res := NewResolver(templates, nil)

	at := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	got, ok := res.RateFor("", "m", at)
	if !ok {
		t.Fatal("expected template 0 to resolve")
	}
	if got != base {
		t.Fatalf("got %+v, want %+v", got, base)
	}

	// Unknown model has no templates → not found (no catalog).
	if _, ok := res.RateFor("", "other", at); ok {
		t.Fatal("expected not found for unknown model")
	}
}

func TestRateFor_CoverageWinsLatestStart(t *testing.T) {
	r0 := r(0, 0, 0, 0)
	r1 := r(1, 1, 1, 1)
	r2 := r(2, 2, 2, 2)
	start1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end1 := tp(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC))
	start2 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	templates := []PricingTemplate{
		{ID: "t0", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r0},
		{ID: "c1", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r1, Start: start1, End: end1},
		{ID: "c2", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r2, Start: start2}, // End nil
	}
	res := NewResolver(templates, nil)

	// Feb 15 is covered by both c1 and c2 → latest Start (c2) wins.
	at := time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC)
	got, ok := res.RateFor("", "m", at)
	if !ok {
		t.Fatal("expected coverage template")
	}
	if got != r2 {
		t.Fatalf("got %+v, want latest-start %+v", got, r2)
	}

	// Jan 15 only covered by c1 → r1.
	got, _ = res.RateFor("", "m", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))
	if got != r1 {
		t.Fatalf("got %+v, want %+v", got, r1)
	}
}

func TestRateFor_CoverageSkippedWhenInactive(t *testing.T) {
	r0 := r(9, 9, 9, 9)
	rf := r(7, 7, 7, 7)
	rp := r(8, 8, 8, 8)
	templates := []PricingTemplate{
		{ID: "t0", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r0},
		{ID: "future", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: rf,
			Start: time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)}, // future Start
		{ID: "past", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: rp,
			Start: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			End:   tp(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC))}, // past End
	}
	res := NewResolver(templates, nil)

	at := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	got, ok := res.RateFor("", "m", at)
	if !ok {
		t.Fatal("expected template 0 fallback")
	}
	if got != r0 {
		t.Fatalf("got %+v, want template0 %+v", got, r0)
	}
}

func TestRateFor_ScheduleTimezone(t *testing.T) {
	// Period: Sundays 20:00–23:00 in America/New_York (EST = UTC-5 in Jan).
	peak := r(100, 200, 300, 400)
	base := r(1, 2, 3, 4)
	templates := []PricingTemplate{
		{
			ID: "sched", Model: "m", Mode: ModeSchedule, Timezone: "America/New_York",
			BaseRates: base,
			Periods: []PricingPeriod{
				{Weekdays: []int{7}, Start: "20:00", End: "23:00", Rates: peak},
			},
		},
	}
	res := NewResolver(templates, nil)

	cases := []struct {
		name string
		at   time.Time
		want usage.CostRates
	}{
		// 01:00 UTC = Sun 20:00 EST → in window → peak (proves timezone + weekday).
		{"in-window", time.Date(2024, 1, 15, 1, 0, 0, 0, time.UTC), peak},
		{"mid-window", time.Date(2024, 1, 15, 2, 30, 0, 0, time.UTC), peak},
		// 04:00 UTC = Sun 23:00 EST → half-open end → base.
		{"at-end", time.Date(2024, 1, 15, 4, 0, 0, 0, time.UTC), base},
		// 00:00 UTC = Sun 19:00 EST → before window → base.
		{"before-window", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), base},
		// 01:00 UTC on a Tuesday (2024-01-16) = Mon 20:00 EST → weekday mismatch → base.
		{"wrong-weekday", time.Date(2024, 1, 16, 1, 0, 0, 0, time.UTC), base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := res.RateFor("", "m", tc.at)
			if !ok {
				t.Fatal("expected resolution")
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRateFor_EndOfDay2400(t *testing.T) {
	// Period: Mondays 22:00–24:00 at +00:00 — the last minute of the day must
	// resolve to the period rate, not silently fall back to base rates.
	peak := r(100, 200, 300, 400)
	base := r(1, 2, 3, 4)
	templates := []PricingTemplate{
		{
			ID: "sched", Model: "m", Mode: ModeSchedule, Timezone: "+00:00",
			BaseRates: base,
			Periods: []PricingPeriod{
				{Weekdays: []int{1}, Start: "22:00", End: "24:00", Rates: peak},
			},
		},
	}
	res := NewResolver(templates, nil)

	cases := []struct {
		name string
		at   time.Time
		want usage.CostRates
	}{
		{"at-start", time.Date(2024, 1, 15, 22, 0, 0, 0, time.UTC), peak},
		{"last-minute", time.Date(2024, 1, 15, 23, 59, 0, 0, time.UTC), peak},
		{"next-day-midnight", time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC), base},
		{"before-window", time.Date(2024, 1, 15, 21, 59, 0, 0, time.UTC), base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := res.RateFor("", "m", tc.at)
			if !ok {
				t.Fatal("expected resolution")
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseFixedUTCOffset(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		canonical string
		offset    int
		matched   bool
		wantErr   bool
	}{
		{name: "positive hour", input: "+8", canonical: "+08:00", offset: 8 * 60 * 60, matched: true},
		{name: "positive minute offset", input: "+05:30", canonical: "+05:30", offset: 5*60*60 + 30*60, matched: true},
		{name: "negative hour", input: "-12", canonical: "-12:00", offset: -12 * 60 * 60, matched: true},
		{name: "maximum offset", input: "+14:00", canonical: "+14:00", offset: 14 * 60 * 60, matched: true},
		{name: "utc prefix", input: "UTC-5", canonical: "-05:00", offset: -5 * 60 * 60, matched: true},
		{name: "zero canonical sign", input: "-00:00", canonical: "+00:00", offset: 0, matched: true},
		{name: "legacy IANA", input: "Asia/Shanghai", matched: false},
		{name: "below minimum", input: "-12:01", matched: true, wantErr: true},
		{name: "above maximum", input: "+14:01", matched: true, wantErr: true},
		{name: "bad minutes", input: "+08:60", matched: true, wantErr: true},
		{name: "missing sign", input: "8", matched: false},
		{name: "bad format", input: "+8:0", matched: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			canonical, offset, matched, err := parseFixedUTCOffset(tc.input)
			if matched != tc.matched {
				t.Fatalf("matched = %v, want %v (err=%v)", matched, tc.matched, err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if canonical != tc.canonical || offset != tc.offset {
				t.Fatalf("parsed %q as (%q, %d), want (%q, %d)", tc.input, canonical, offset, tc.canonical, tc.offset)
			}
		})
	}
}

func TestNormalizeTemplateCanonicalizesFixedOffsetAndPreservesLegacyIANA(t *testing.T) {
	fixed := PricingTemplate{Timezone: " UTC+8 "}
	NormalizeTemplate(&fixed)
	if fixed.Timezone != "+08:00" {
		t.Fatalf("fixed timezone = %q, want %q", fixed.Timezone, "+08:00")
	}

	legacy := PricingTemplate{Timezone: " Asia/Shanghai "}
	NormalizeTemplate(&legacy)
	if legacy.Timezone != "Asia/Shanghai" {
		t.Fatalf("legacy timezone = %q, want %q", legacy.Timezone, "Asia/Shanghai")
	}
}

func TestRateFor_ScheduleFixedOffset(t *testing.T) {
	peak := r(10, 10, 10, 10)
	base := r(1, 1, 1, 1)
	res := NewResolver([]PricingTemplate{{
		Model: "m", Mode: ModeSchedule, Timezone: "+08:00", BaseRates: base,
		Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "09:00", End: "10:00", Rates: peak}},
	}}, nil)

	// Monday 09:00 at UTC+08 is Monday 01:00 UTC.
	if got, ok := res.RateFor("", "m", time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)); !ok || got != peak {
		t.Fatalf("fixed-offset in-window result = (%+v, %v), want peak", got, ok)
	}
	// Monday 08:59 at UTC+08 is Monday 00:59 UTC, before the window.
	if got, ok := res.RateFor("", "m", time.Date(2024, 1, 1, 0, 59, 0, 0, time.UTC)); !ok || got != base {
		t.Fatalf("fixed-offset before-window result = (%+v, %v), want base", got, ok)
	}
}

func TestRateFor_CaseInsensitive(t *testing.T) {
	rates := r(5, 6, 7, 8)
	templates := []PricingTemplate{
		{ID: "t", Target: "OpenAI", Model: "GPT-4o", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: rates},
	}
	res := NewResolver(templates, nil)

	at := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	got, ok := res.RateFor("openai", "gpt-4o", at)
	if !ok {
		t.Fatal("expected case-insensitive match")
	}
	if got != rates {
		t.Fatalf("got %+v, want %+v", got, rates)
	}
}

func TestRateFor_TargetFallbackChain(t *testing.T) {
	defT0 := r(1, 1, 1, 1)
	specificT0 := r(2, 2, 2, 2)
	templates := []PricingTemplate{
		{ID: "def", Target: "", Model: "chain", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: defT0},
		{ID: "spec", Target: "specific", Model: "chain", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: specificT0},
	}
	res := NewResolver(templates, nil)
	at := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	// Exact target hit.
	if got, _ := res.RateFor("specific", "chain", at); got != specificT0 {
		t.Fatalf("got %+v, want specific %+v", got, specificT0)
	}
	// Unknown target falls back to default target.
	if got, _ := res.RateFor("unknown", "chain", at); got != defT0 {
		t.Fatalf("got %+v, want default %+v", got, defT0)
	}
	// Default target directly.
	if got, _ := res.RateFor("", "chain", at); got != defT0 {
		t.Fatalf("got %+v, want default %+v", got, defT0)
	}
	// No templates anywhere, no catalog → not found.
	if _, ok := res.RateFor("unknown", "missing", at); ok {
		t.Fatal("expected not found")
	}
	// Empty model → not found.
	if _, ok := res.RateFor("", "", at); ok {
		t.Fatal("expected not found for empty model")
	}
}

func TestRateFor_CatalogFallback(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	model, want := uniqueCatalogModel(t, cat)

	// No templates → catalog default fallback.
	res := NewResolver(nil, cat)
	at := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	got, ok := res.RateFor("any-target", model, at)
	if !ok {
		t.Fatalf("expected catalog default for %q", model)
	}
	if got != want {
		t.Fatalf("got %+v, want catalog default %+v", got, want)
	}
}

func TestValidateTemplate(t *testing.T) {
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	endAfter := tp(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC))
	endBefore := tp(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	validFlat := PricingTemplate{ID: "ok", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1)}
	validSched := PricingTemplate{
		ID: "ok", Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1),
		Start: start, End: endAfter,
		Periods: []PricingPeriod{
			{Weekdays: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", Rates: r(2, 2, 2, 2)},
			{Weekdays: []int{1}, Start: "18:00", End: "19:00", Rates: r(3, 3, 3, 3)},
		},
	}

	cases := []struct {
		name    string
		tpl     PricingTemplate
		wantErr bool
	}{
		{"valid flat template0", validFlat, false},
		{"valid schedule", validSched, false},
		{"empty model", PricingTemplate{Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1)}, true},
		{"unknown mode", PricingTemplate{Model: "m", Mode: "hourly", Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1)}, true},
		{"invalid timezone", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: "Mars/Phobos", BaseRates: r(1, 1, 1, 1)}, true},
		{"negative input rate", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(-1, 1, 1, 1)}, true},
		{"negative output rate", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, -1, 1, 1)}, true},
		{"negative cache write rate", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, 1, -1, 1)}, true},
		{"negative cache read rate", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, -1)}, true},
		{"end not after start", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, End: endBefore}, true},
		{"template0 with end", PricingTemplate{Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), End: endAfter}, true},
		{"template0 with schedule mode", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1)}, true},
		{"bad period start", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "abc", End: "17:00", Rates: r(2, 2, 2, 2)}}}, true},
		{"bad period end", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "09:00", End: "17:60", Rates: r(2, 2, 2, 2)}}}, true},
		{"cross-midnight period", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "22:00", End: "02:00", Rates: r(2, 2, 2, 2)}}}, true},
		{"invalid weekday", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{0}, Start: "09:00", End: "17:00", Rates: r(2, 2, 2, 2)}}}, true},
		{"overlapping periods same weekday", PricingTemplate{
			Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start,
			Periods: []PricingPeriod{
				{Weekdays: []int{1}, Start: "09:00", End: "12:00", Rates: r(2, 2, 2, 2)},
				{Weekdays: []int{1}, Start: "11:00", End: "13:00", Rates: r(3, 3, 3, 3)},
			},
		}, true},
		{"adjacent periods allowed", PricingTemplate{
			Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start,
			Periods: []PricingPeriod{
				{Weekdays: []int{1}, Start: "09:00", End: "12:00", Rates: r(2, 2, 2, 2)},
				{Weekdays: []int{1}, Start: "12:00", End: "13:00", Rates: r(3, 3, 3, 3)},
			},
		}, false},
		{"end 24:00 allowed", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "22:00", End: "24:00", Rates: r(2, 2, 2, 2)}}}, false},
		{"start 24:00 rejected", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "24:00", End: "24:00", Rates: r(2, 2, 2, 2)}}}, true},
		{"invalid 24:01 end rejected", PricingTemplate{Model: "m", Mode: ModeSchedule, Timezone: DefaultTimezone, BaseRates: r(1, 1, 1, 1), Start: start, Periods: []PricingPeriod{{Weekdays: []int{1}, Start: "22:00", End: "24:01", Rates: r(2, 2, 2, 2)}}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTemplate(&tc.tpl)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateCoverageSet(t *testing.T) {
	s1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s2 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	s3 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		tpls    []PricingTemplate
		wantErr bool
	}{
		{
			name: "overlapping coverage",
			tpls: []PricingTemplate{
				{ID: "a", Model: "m", Mode: ModeFlat, Start: s1, End: tp(s3)},
				{ID: "b", Model: "m", Mode: ModeFlat, Start: s2}, // End nil overlaps [s1,s3)
			},
			wantErr: true,
		},
		{
			name: "adjacent allowed",
			tpls: []PricingTemplate{
				{ID: "a", Model: "m", Mode: ModeFlat, Start: s1, End: tp(s2)},
				{ID: "b", Model: "m", Mode: ModeFlat, Start: s2, End: tp(s3)},
			},
			wantErr: false,
		},
		{
			name: "template0 ignored",
			tpls: []PricingTemplate{
				{ID: "t0", Model: "m", Mode: ModeFlat, BaseRates: r(1, 1, 1, 1)},
				{ID: "a", Model: "m", Mode: ModeFlat, Start: s1, End: tp(s3)},
			},
			wantErr: false,
		},
		{
			name: "different targets do not conflict",
			tpls: []PricingTemplate{
				{ID: "a", Target: "x", Model: "m", Mode: ModeFlat, Start: s1},
				{ID: "b", Target: "y", Model: "m", Mode: ModeFlat, Start: s1},
			},
			wantErr: false,
		},
		{
			name: "future start participates",
			tpls: []PricingTemplate{
				{ID: "a", Model: "m", Mode: ModeFlat, Start: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), End: tp(time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))},
				{ID: "b", Model: "m", Mode: ModeFlat, Start: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCoverageSet(tc.tpls)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestNormalizeTemplate(t *testing.T) {
	tpl := PricingTemplate{
		Target:   "  OpenAI  ",
		Model:    "  GPT-4o ",
		Mode:     "",
		Timezone: "   ",
		Periods:  []PricingPeriod{{Start: " 09:00 ", End: " 17:00 "}},
	}
	NormalizeTemplate(&tpl)

	if tpl.Target != "OpenAI" {
		t.Fatalf("target = %q, want %q", tpl.Target, "OpenAI")
	}
	if tpl.Model != "GPT-4o" {
		t.Fatalf("model = %q, want %q (case preserved)", tpl.Model, "GPT-4o")
	}
	if tpl.Mode != ModeFlat {
		t.Fatalf("mode = %q, want %q", tpl.Mode, ModeFlat)
	}
	if tpl.Timezone != DefaultTimezone {
		t.Fatalf("timezone = %q, want %q", tpl.Timezone, DefaultTimezone)
	}
	if tpl.Periods[0].Start != "09:00" || tpl.Periods[0].End != "17:00" {
		t.Fatalf("period not trimmed: %+v", tpl.Periods[0])
	}
}

func TestPricingCache(t *testing.T) {
	r1 := r(1, 1, 1, 1)
	r2 := r(2, 2, 2, 2)

	var current []PricingTemplate
	load := func() ([]PricingTemplate, error) { return current, nil }
	c := NewPricingCache(load, nil)

	current = []PricingTemplate{{ID: "t", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r1}}
	at := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	if got, _ := c.Get().RateFor("", "m", at); got != r1 {
		t.Fatalf("first Get: got %+v, want %+v", got, r1)
	}

	// Mutate without invalidating → cached resolver still returns r1.
	current = []PricingTemplate{{ID: "t", Model: "m", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: r2}}
	if got, _ := c.Get().RateFor("", "m", at); got != r1 {
		t.Fatalf("stale Get: got %+v, want cached %+v", got, r1)
	}

	// Invalidate → rebuild reflects new templates.
	c.Invalidate()
	if got, _ := c.Get().RateFor("", "m", at); got != r2 {
		t.Fatalf("after Invalidate: got %+v, want %+v", got, r2)
	}
}

func TestPricingCache_NilInputs(t *testing.T) {
	c := NewPricingCache(nil, nil)
	res := c.Get()
	if res == nil {
		t.Fatal("expected non-nil resolver")
	}
	if _, ok := res.RateFor("", "m", time.Now()); ok {
		t.Fatal("expected not found with empty inputs")
	}
}

func TestPricingCacheRetriesAfterTemplateLoadError(t *testing.T) {
	want := r(7, 8, 9, 10)
	loadCalls := 0
	cache := NewPricingCache(func() ([]PricingTemplate, error) {
		loadCalls++
		if loadCalls == 1 {
			return nil, errors.New("temporary storage failure")
		}
		return []PricingTemplate{{Model: "recovering-model", Mode: ModeFlat, Timezone: DefaultTimezone, BaseRates: want}}, nil
	}, nil)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, ok := cache.Get().RateFor("", "recovering-model", at); ok {
		t.Fatal("unexpected rate after template load failure")
	}
	if got, ok := cache.Get().RateFor("", "recovering-model", at); !ok || got != want {
		t.Fatalf("recovered rate=(%+v,%v), want (%+v,true); loads=%d", got, ok, want, loadCalls)
	}
	if loadCalls != 2 {
		t.Fatalf("load called %d times, want retry after first failure", loadCalls)
	}
}

func TestRateForProviderSpecificCatalogFallback(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	var openAIRates, azureRates usage.CostRates
	var openAIModel, azureModel string
	for _, entry := range cat.ListAll() {
		if !strings.EqualFold(entry.Model, "o1-preview") || entry.DefaultCost == nil {
			continue
		}
		rates := usage.CostRates{
			InputPer1MTokens:     entry.DefaultCost.InputPer1MTokens,
			OutputPer1MTokens:    entry.DefaultCost.OutputPer1MTokens,
			CacheWritePer1MToken: entry.DefaultCost.CacheWritePer1MToken,
			CacheReadPer1MToken:  entry.DefaultCost.CacheReadPer1MToken,
		}
		switch entry.EndpointType {
		case "openai":
			openAIModel, openAIRates = entry.Model, rates
		case "azure_openai":
			azureModel, azureRates = entry.Model, rates
		}
	}
	if openAIModel == "" || azureModel == "" {
		t.Skip("catalog no longer contains provider-specific o1-preview entries")
	}
	if openAIRates == azureRates {
		t.Fatal("catalog fixture must have different provider prices")
	}
	cache := NewTargetPricingCache(nil, cat, func() (map[string]string, error) {
		return map[string]string{"openai-target": "openai", "azure-target": "azure_openai"}, nil
	})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		target string
		model  string
		want   usage.CostRates
	}{{"openai-target", openAIModel, openAIRates}, {"azure-target", azureModel, azureRates}} {
		got, ok := cache.Get().RateFor(tc.target, tc.model, at)
		if !ok || got != tc.want {
			t.Errorf("RateFor(%q,%q)=(%+v,%v), want (%+v,true)", tc.target, tc.model, got, ok, tc.want)
		}
	}
}

// uniqueCatalogModel finds a model that appears exactly once in the catalog
// with a non-nil default cost, so the catalog-fallback test is unambiguous.
func uniqueCatalogModel(t *testing.T, cat *catalog.Catalog) (string, usage.CostRates) {
	t.Helper()
	count := map[string]int{}
	for _, e := range cat.ListAll() {
		count[strings.ToLower(strings.TrimSpace(e.Model))]++
	}
	for _, e := range cat.ListAll() {
		m := strings.ToLower(strings.TrimSpace(e.Model))
		if e.DefaultCost != nil && m != "" && count[m] == 1 {
			return m, usage.CostRates{
				InputPer1MTokens:     e.DefaultCost.InputPer1MTokens,
				OutputPer1MTokens:    e.DefaultCost.OutputPer1MTokens,
				CacheWritePer1MToken: e.DefaultCost.CacheWritePer1MToken,
				CacheReadPer1MToken:  e.DefaultCost.CacheReadPer1MToken,
			}
		}
	}
	t.Skip("no unique catalog model with default cost")
	return "", usage.CostRates{}
}

func TestRateFor_ScopedGPUMissDoesNotInheritGenericFallback(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	generic := usage.CostRates{InputPer1MTokens: 7, OutputPer1MTokens: 21}
	other := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 5}
	// No provider-specific entries at all: both GPT targets miss the provider
	// fallback and the generic map holds a same-name price inherited from
	// another provider's catalog entry.
	r := &Resolver{
		targetProviders: map[string]string{
			"openai-target": config.EndpointTypeOpenAI,
			"azure-target":  config.EndpointTypeAzureOpenAI,
		},
		fallback: map[string]usage.CostRates{
			"gpt-synth":   generic,
			"other-synth": other,
		},
	}

	if _, ok := r.RateFor("openai-target", "gpt-synth", at); ok {
		t.Fatal("scoped GPT lookup on an openai target must not inherit the generic same-name price")
	}
	if _, ok := r.RateFor("azure-target", "gpt-synth", at); ok {
		t.Fatal("scoped GPT lookup on an azure target must not inherit the generic same-name price")
	}
	if _, ok := r.RateFor("openai-target", "other-synth", at); !ok {
		t.Fatal("non-GPT catalog fallback must still apply for openai targets")
	}
	if got, ok := r.RateFor("", "gpt-synth", at); !ok || got != generic.Scaled(1) {
		t.Fatalf("unscoped GPT lookup should use the generic fallback, got %+v ok=%v", got, ok)
	}
}
