package quota

import (
	"fmt"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// DimensionDaily/Weekly/Monthly 为规范常量。
const (
	DimensionDaily   = config.QuotaDimensionDaily
	DimensionWeekly  = config.QuotaDimensionWeekly
	DimensionMonthly = config.QuotaDimensionMonthly
)

// PeriodRange 返回 given dimension + now 的 (from, to) 聚合区间。to 固定传 now (UTC)。
// 时区固定 UTC。禁止字符串拼接后 Parse。
//
// 周期规则：
//   - daily:   当天零点 UTC → now
//   - weekly:  ISO 8601，周一（Go Weekday: Sunday=0, offset = (Weekday+6)%7）
//   - monthly: 当月1号零点 UTC → now
func PeriodRange(dimension string, now time.Time) (from, to time.Time, err error) {
	now = now.UTC()
	switch dimension {
	case DimensionDaily:
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case DimensionWeekly:
		// ISO 8601: Monday is day 1. Go's Weekday: Sunday=0, Monday=1..Saturday=6.
		// offset = (Weekday + 6) % 7 → Monday=0, Tuesday=1..Sunday=6.
		offset := int(now.Weekday()+6) % 7
		monday := now.AddDate(0, 0, -offset)
		from = time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
	case DimensionMonthly:
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("quota: unknown dimension %q", dimension)
	}
	return from, now, nil
}

// NextResetAt 返回下次重置时刻（零点 UTC）。
//   - daily:   tomorrow 零点
//   - weekly:  下个周一零点
//   - monthly: 次月1号零点
func NextResetAt(dimension string, now time.Time) (time.Time, error) {
	now = now.UTC()
	switch dimension {
	case DimensionDaily:
		tomorrow := now.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC), nil
	case DimensionWeekly:
		offset := int(now.Weekday()+6) % 7
		nextMonday := now.AddDate(0, 0, 7-offset)
		return time.Date(nextMonday.Year(), nextMonday.Month(), nextMonday.Day(), 0, 0, 0, 0, time.UTC), nil
	case DimensionMonthly:
		// Compute 1st of next month directly via time.Date's month-overflow handling.
		// Avoids AddDate(0, 1, 0) which normalizes Aug-31 → Oct-01 (Sep 31 doesn't exist).
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC), nil
	default:
		return time.Time{}, fmt.Errorf("quota: unknown dimension %q", dimension)
	}
}
