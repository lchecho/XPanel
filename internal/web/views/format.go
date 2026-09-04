package views

import (
	"fmt"
	"math"
	"time"
)

// FormatBytes 按 http.md 显示约定输出 IEC 二进制单位：≥1 MiB 保留两位小数（四舍五入），KiB 取整。
func FormatBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const (
		kib = 1024
		mib = kib * 1024
		gib = mib * 1024
		tib = gib * 1024
	)
	switch {
	case value >= tib:
		return fmt.Sprintf("%.2f TiB", roundHalfUp(float64(value)/tib))
	case value >= gib:
		return fmt.Sprintf("%.2f GiB", roundHalfUp(float64(value)/gib))
	case value >= mib:
		return fmt.Sprintf("%.2f MiB", roundHalfUp(float64(value)/mib))
	case value >= kib:
		return fmt.Sprintf("%d KiB", int64(math.Round(float64(value)/kib)))
	default:
		return fmt.Sprintf("%d B", value)
	}
}

func roundHalfUp(value float64) float64 { return math.Floor(value*100+0.5) / 100 }

// FormatTime 按面板配额时区渲染 `YYYY-MM-DD HH:MM:SS`；nil 返回空串。
func FormatTime(value *time.Time, location *time.Location) string {
	if value == nil || value.IsZero() {
		return ""
	}
	if location == nil {
		location = time.UTC
	}
	return value.In(location).Format("2006-01-02 15:04:05")
}

func FormatTimeValue(value time.Time, location *time.Location) string {
	return FormatTime(&value, location)
}

// Location 加载 IANA 时区，失败时退回 UTC。
func Location(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return location
}
