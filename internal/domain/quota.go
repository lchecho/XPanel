package domain

import (
	"fmt"
	"time"
)

// MaxLimitBytes 是配额上限（data-model §QuotaPolicy）。
const MaxLimitBytes int64 = 1 << 62

// ValidateQuotaPolicy 校验配额策略：null 表示无限，否则必须为正且不超过 2^62；重置日 1–28。
func ValidateQuotaPolicy(limit *int64, resetDay int) error {
	if resetDay < 1 || resetDay > 28 {
		return &ValidationError{Field: "reset_day", Message: "reset day must be between 1 and 28"}
	}
	if limit != nil && (*limit <= 0 || *limit > MaxLimitBytes) {
		return &ValidationError{Field: "limit_bytes", Message: "quota must be positive and at most 2^62 bytes"}
	}
	return nil
}

// IsQuotaExceeded 判定 accounted 总用量是否达到或超过配额；无限配额永不超限（spec Edge Cases）。
func IsQuotaExceeded(limit *int64, accountedUplink, accountedDownlink int64) bool {
	if limit == nil {
		return false
	}
	total, overflow := addBytes(accountedUplink, accountedDownlink)
	if overflow {
		return true
	}
	return total >= *limit
}

// addBytes 在写入前检查 int64 溢出（data-model §Conventions）。
func addBytes(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a > MaxInt64-b {
		return 0, true
	}
	return a + b, false
}

const MaxInt64 = int64(^uint64(0) >> 1)

// CycleBounds 返回包含 now 的自然月式周期 [start, end)，边界为面板时区中 resetDay 日 00:00，
// 结果以 UTC 返回。使用 time.Date 在 Location 中构造以正确跨越夏令时。
func CycleBounds(now time.Time, location *time.Location, resetDay int) (time.Time, time.Time, error) {
	if location == nil {
		location = time.UTC
	}
	if resetDay < 1 || resetDay > 28 {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid reset day %d", resetDay)
	}
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), resetDay, 0, 0, 0, 0, location)
	if local.Before(start) {
		start = time.Date(local.Year(), local.Month()-1, resetDay, 0, 0, 0, 0, location)
	}
	end := NextCycleStart(start, location, resetDay)
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid quota cycle")
	}
	return start.UTC(), end.UTC(), nil
}

// NextCycleStart 返回 start 之后下一个周期边界（下月同一 resetDay 的本地 00:00）。
func NextCycleStart(start time.Time, location *time.Location, resetDay int) time.Time {
	if location == nil {
		location = time.UTC
	}
	local := start.In(location)
	return time.Date(local.Year(), local.Month()+1, resetDay, 0, 0, 0, 0, location).UTC()
}

// DayBounds 返回 at 所在自然日的 UTC 起点与本地日期字符串（按周期时区快照计算）。
func DayBounds(at time.Time, location *time.Location) (time.Time, string) {
	if location == nil {
		location = time.UTC
	}
	local := at.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return start.UTC(), start.Format("2006-01-02")
}
