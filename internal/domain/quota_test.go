package domain

import (
	"testing"
	"time"
)

func TestCycleBoundsHandlesMonthLengthsAndResetDay28(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	cases := []struct {
		now       string
		resetDay  int
		wantStart string
		wantEnd   string
	}{
		{"2026-02-10T10:00:00+08:00", 28, "2026-01-28T00:00:00+08:00", "2026-02-28T00:00:00+08:00"},
		{"2026-02-28T00:00:00+08:00", 28, "2026-02-28T00:00:00+08:00", "2026-03-28T00:00:00+08:00"},
		{"2026-01-31T23:59:59+08:00", 1, "2026-01-01T00:00:00+08:00", "2026-02-01T00:00:00+08:00"},
		{"2026-12-15T00:00:00+08:00", 15, "2026-12-15T00:00:00+08:00", "2027-01-15T00:00:00+08:00"},
	}
	for _, c := range cases {
		now, _ := time.Parse(time.RFC3339, c.now)
		start, end, err := CycleBounds(now, loc, c.resetDay)
		if err != nil {
			t.Fatal(err)
		}
		wantStart, _ := time.Parse(time.RFC3339, c.wantStart)
		wantEnd, _ := time.Parse(time.RFC3339, c.wantEnd)
		if !start.Equal(wantStart) || !end.Equal(wantEnd) {
			t.Fatalf("%s reset %d: got [%s, %s) want [%s, %s)", c.now, c.resetDay, start, end, wantStart, wantEnd)
		}
	}
	if _, _, err := CycleBounds(time.Now(), loc, 29); err == nil {
		t.Fatal("reset day 29 accepted")
	}
}

func TestCycleBoundsAcrossDaylightSaving(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	now, _ := time.Parse(time.RFC3339, "2026-03-20T12:00:00+01:00")
	start, end, err := CycleBounds(now, loc, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 三月边界在 CET，四月边界在 CEST：周期长度比 31 天少 1 小时。
	if end.Sub(start) != 31*24*time.Hour-time.Hour {
		t.Fatalf("DST cycle length = %s", end.Sub(start))
	}
	if end.In(loc).Hour() != 0 || start.In(loc).Hour() != 0 {
		t.Fatalf("boundaries are not local midnight: %s %s", start.In(loc), end.In(loc))
	}
}

func TestQuotaExceededAndValidation(t *testing.T) {
	limit := int64(1000)
	if !IsQuotaExceeded(&limit, 600, 400) {
		t.Fatal("usage equal to the limit must be exceeded")
	}
	if IsQuotaExceeded(&limit, 600, 399) {
		t.Fatal("usage below the limit reported as exceeded")
	}
	if IsQuotaExceeded(nil, MaxInt64, MaxInt64) {
		t.Fatal("unlimited quota must never be exceeded")
	}
	if !IsQuotaExceeded(&limit, MaxInt64, 1) {
		t.Fatal("overflowing sum must be treated as exceeded")
	}
	zero := int64(0)
	if err := ValidateQuotaPolicy(&zero, 1); err == nil {
		t.Fatal("zero limit accepted")
	}
	huge := MaxLimitBytes + 1
	if err := ValidateQuotaPolicy(&huge, 1); err == nil {
		t.Fatal("limit above 2^62 accepted")
	}
	if err := ValidateQuotaPolicy(nil, 0); err == nil {
		t.Fatal("reset day 0 accepted")
	}
	if err := ValidateQuotaPolicy(nil, 28); err != nil {
		t.Fatal(err)
	}
}

func TestDayBoundsUseCycleTimezone(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	at, _ := time.Parse(time.RFC3339, "2026-09-04T17:30:00Z") // 01:30 next day in Shanghai
	start, local := DayBounds(at, loc)
	if local != "2026-09-05" || start.Format(time.RFC3339) != "2026-09-04T16:00:00Z" {
		t.Fatalf("day bounds = %s %s", start, local)
	}
}
