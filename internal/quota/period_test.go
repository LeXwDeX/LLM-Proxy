package quota

import (
	"testing"
	"time"
)

func TestPeriodRange(t *testing.T) {
	// UTC helper.
	utc := func(y int, m time.Month, d, h, min, s int) time.Time {
		return time.Date(y, m, d, h, min, s, 0, time.UTC)
	}

	// Daily: midday.
	t.Run("daily_midday", func(t *testing.T) {
		now := utc(2026, 6, 8, 15, 30, 0)
		from, to, err := PeriodRange(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 8, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
		if !to.Equal(now) {
			t.Errorf("to: got %v, want %v", to, now)
		}
	})

	// Daily: near midnight.
	t.Run("daily_near_midnight", func(t *testing.T) {
		now := utc(2026, 6, 8, 23, 59, 59)
		from, to, err := PeriodRange(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 8, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
		if !to.Equal(now) {
			t.Errorf("to: got %v, want %v", to, now)
		}
	})

	// Daily: very beginning of month.
	t.Run("daily_month_start", func(t *testing.T) {
		now := utc(2026, 6, 1, 0, 1, 0)
		from, _, err := PeriodRange(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 1, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
	})

	// Daily: last day of month.
	t.Run("daily_month_end", func(t *testing.T) {
		now := utc(2026, 6, 30, 23, 59, 0)
		from, _, err := PeriodRange(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 30, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
	})

	// Weekly: Monday (ISO week starts).
	t.Run("weekly_monday", func(t *testing.T) {
		now := utc(2026, 6, 8, 12, 0, 0) // 2026-06-08 is a Monday.
		from, to, err := PeriodRange(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 8, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
		if !to.Equal(now) {
			t.Errorf("to: got %v, want %v", to, now)
		}
	})

	// Weekly: Sunday (last day of ISO week).
	t.Run("weekly_sunday", func(t *testing.T) {
		now := utc(2026, 6, 7, 12, 0, 0) // 2026-06-07 is a Sunday.
		from, _, err := PeriodRange(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 1, 0, 0, 0) // last Monday.
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v (previous Monday)", from, wantFrom)
		}
	})

	// Weekly: Wednesday.
	t.Run("weekly_wednesday", func(t *testing.T) {
		now := utc(2026, 6, 10, 15, 0, 0) // 2026-06-10 is a Wednesday.
		from, _, err := PeriodRange(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 8, 0, 0, 0) // this Monday.
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v (Monday)", from, wantFrom)
		}
	})

	// Monthly: mid-month.
	t.Run("monthly_mid", func(t *testing.T) {
		now := utc(2026, 6, 15, 10, 0, 0)
		from, _, err := PeriodRange(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 6, 1, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
	})

	// Monthly: Dec 31 → next month January.
	t.Run("monthly_dec_cross_year", func(t *testing.T) {
		now := utc(2026, 12, 31, 23, 59, 0)
		from, _, err := PeriodRange(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 12, 1, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
	})

	// Monthly: Aug-31 → still Aug.
	t.Run("monthly_aug31", func(t *testing.T) {
		now := utc(2026, 8, 31, 12, 0, 0)
		from, _, err := PeriodRange(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		wantFrom := utc(2026, 8, 1, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
	})

	// Unknown dimension → err.
	t.Run("unknown", func(t *testing.T) {
		_, _, err := PeriodRange("yearly", utc(2026, 6, 8, 12, 0, 0))
		if err == nil {
			t.Error("expected err, got nil")
		}
	})

	// Non-UTC input → output must be UTC.
	t.Run("timezone_forced_utc", func(t *testing.T) {
		shanghai, _ := time.LoadLocation("Asia/Shanghai")
		now := time.Date(2026, 6, 8, 15, 30, 0, 0, shanghai)
		from, to, err := PeriodRange(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if from.Location() != time.UTC {
			t.Errorf("from.Location: got %v, want UTC", from.Location())
		}
		if to.Location() != time.UTC {
			t.Errorf("to.Location: got %v, want UTC", to.Location())
		}
		// 2026-06-08 15:30 Shanghai = 2026-06-08 07:30 UTC → today UTC = June 8.
		wantFrom := utc(2026, 6, 8, 0, 0, 0)
		if !from.Equal(wantFrom) {
			t.Errorf("from: got %v, want %v", from, wantFrom)
		}
	})

	// Non-UTC input for weekly → from/to must be UTC.
	t.Run("timezone_forced_utc_weekly", func(t *testing.T) {
		shanghai, _ := time.LoadLocation("Asia/Shanghai")
		now := time.Date(2026, 6, 12, 15, 30, 0, 0, shanghai) // Thursday.
		from, to, err := PeriodRange(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if from.Location() != time.UTC {
			t.Errorf("from.Location: got %v, want UTC", from.Location())
		}
		if to.Location() != time.UTC {
			t.Errorf("to.Location: got %v, want UTC", to.Location())
		}
	})
}

func TestNextResetAt(t *testing.T) {
	utc := func(y int, m time.Month, d, h, min, s int) time.Time {
		return time.Date(y, m, d, h, min, s, 0, time.UTC)
	}

	// Daily: tomorrow midnight.
	t.Run("daily_midday", func(t *testing.T) {
		now := utc(2026, 6, 8, 15, 30, 0)
		reset, err := NextResetAt(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 6, 9, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	t.Run("daily_near_midnight", func(t *testing.T) {
		now := utc(2026, 6, 8, 23, 59, 59)
		reset, err := NextResetAt(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 6, 9, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	t.Run("daily_month_end", func(t *testing.T) {
		now := utc(2026, 6, 30, 23, 59, 0)
		reset, err := NextResetAt(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 7, 1, 0, 0, 0) // rolls into next month.
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Weekly: Monday → next Monday.
	t.Run("weekly_monday", func(t *testing.T) {
		now := utc(2026, 6, 8, 12, 0, 0)
		reset, err := NextResetAt(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 6, 15, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Weekly: Sunday → this next Monday (next day).
	t.Run("weekly_sunday", func(t *testing.T) {
		now := utc(2026, 6, 7, 12, 0, 0)
		reset, err := NextResetAt(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 6, 8, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Weekly: Wednesday.
	t.Run("weekly_wednesday", func(t *testing.T) {
		now := utc(2026, 6, 10, 15, 0, 0)
		reset, err := NextResetAt(DimensionWeekly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 6, 15, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Monthly: mid-month → 1st of next month.
	t.Run("monthly_mid", func(t *testing.T) {
		now := utc(2026, 6, 15, 10, 0, 0)
		reset, err := NextResetAt(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 7, 1, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Monthly: Dec → Jan of next year.
	t.Run("monthly_dec_cross_year", func(t *testing.T) {
		now := utc(2026, 12, 31, 23, 59, 0)
		reset, err := NextResetAt(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2027, 1, 1, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Monthly: Aug-31 → Sep-1.
	t.Run("monthly_aug31", func(t *testing.T) {
		now := utc(2026, 8, 31, 12, 0, 0)
		reset, err := NextResetAt(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2026, 9, 1, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// Unknown dimension → err.
	t.Run("unknown", func(t *testing.T) {
		_, err := NextResetAt("yearly", utc(2026, 6, 8, 12, 0, 0))
		if err == nil {
			t.Error("expected err, got nil")
		}
	})

	// Non-UTC input → output is UTC.
	t.Run("timezone_forced_utc", func(t *testing.T) {
		shanghai, _ := time.LoadLocation("Asia/Shanghai")
		now := time.Date(2026, 6, 8, 15, 30, 0, 0, shanghai)
		reset, err := NextResetAt(DimensionDaily, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if reset.Location() != time.UTC {
			t.Errorf("reset.Location: got %v, want UTC", reset.Location())
		}
		// 2026-06-08 15:30 Shanghai = 2026-06-08 07:30 UTC → tomorrow UTC = June 9.
		want := utc(2026, 6, 9, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})
}

func TestNextResetAtMonthlyFebBoundary(t *testing.T) {
	utc := func(y int, m time.Month, d, h, min, s int) time.Time {
		return time.Date(y, m, d, h, min, s, 0, time.UTC)
	}

	// 2024 is a leap year: Feb 29 → Mar 1.
	t.Run("feb29_leap", func(t *testing.T) {
		now := utc(2024, 2, 29, 12, 0, 0)
		reset, err := NextResetAt(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2024, 3, 1, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})

	// 2025 is NOT a leap year: Feb 28 → Mar 1.
	t.Run("feb28_non_leap", func(t *testing.T) {
		now := utc(2025, 2, 28, 12, 0, 0)
		reset, err := NextResetAt(DimensionMonthly, now)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := utc(2025, 3, 1, 0, 0, 0)
		if !reset.Equal(want) {
			t.Errorf("reset: got %v, want %v", reset, want)
		}
	})
}
