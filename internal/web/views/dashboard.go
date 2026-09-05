package views

import (
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// DashboardView 是仪表盘 fragment 的展示模型。
type DashboardView struct {
	HealthState      string
	HealthLabel      string
	LastSuccessAt    string
	LastErrorText    string
	LastCollectionAt string
	Stale            bool
	GeneratedAt      string
	TotalUsers       int
	Active           int
	Disabled         int
	QuotaExceeded    int
	Pending          int
	StuckSync        int
	Deleted          int
	AccountedText    string
	Failed           []FailedOperationView
}

type FailedOperationView struct {
	UserID       string
	DisplayName  string
	ReasonLabel  string
	StateLabel   string
	ErrorText    string
	AttemptCount int
	NextAttempt  string
}

func ReasonLabel(reason domain.SyncReason) string {
	switch reason {
	case domain.SyncCreate:
		return "创建"
	case domain.SyncEnable:
		return "启用"
	case domain.SyncDisable:
		return "禁用"
	case domain.SyncQuotaBlock:
		return "配额封禁"
	case domain.SyncQuotaRestore:
		return "配额恢复"
	case domain.SyncRotate:
		return "凭证轮换"
	case domain.SyncDelete:
		return "删除"
	case domain.SyncReconcile:
		return "协调修复"
	default:
		return string(reason)
	}
}

func SyncStateLabel(state domain.SyncState) string {
	switch state {
	case domain.SyncPending:
		return "待执行"
	case domain.SyncLeased:
		return "执行中"
	case domain.SyncRetryWait:
		return "等待重试"
	case domain.SyncSucceeded:
		return "已确认"
	case domain.SyncSuperseded:
		return "已被更新意图取代"
	case domain.SyncPermanentFailed:
		return "永久失败"
	default:
		return string(state)
	}
}

func NewFailedOperationViews(records []ports.FailedOperationRecord, location *time.Location) []FailedOperationView {
	result := make([]FailedOperationView, 0, len(records))
	for _, record := range records {
		result = append(result, FailedOperationView{UserID: record.UserID.String(), DisplayName: record.DisplayName,
			ReasonLabel: ReasonLabel(record.Reason), StateLabel: SyncStateLabel(record.State), ErrorText: ErrorSentence(record.ErrorCode),
			AttemptCount: record.AttemptCount, NextAttempt: FormatTimeValue(record.NextAttemptAt, location)})
	}
	return result
}

// DailyRow 是详情页每日趋势表的一行。
type DailyRow struct {
	LocalDate    string
	UplinkText   string
	DownlinkText string
	TotalText    string
}

func NewDailyRows(records []ports.DailyAggregateRecord) []DailyRow {
	result := make([]DailyRow, 0, len(records))
	for _, record := range records {
		result = append(result, DailyRow{LocalDate: record.LocalDate, UplinkText: FormatBytes(record.UplinkBytes),
			DownlinkText: FormatBytes(record.DownlinkBytes), TotalText: FormatBytes(record.UplinkBytes + record.DownlinkBytes)})
	}
	return result
}

// EventRow 是连续性事件的一行。
type EventRow struct {
	OccurredAt string
	Type       string
	Direction  string
	Sentence   string
}

func NewEventRows(records []ports.ContinuityEventListRecord, location *time.Location) []EventRow {
	result := make([]EventRow, 0, len(records))
	for _, record := range records {
		result = append(result, EventRow{OccurredAt: FormatTimeValue(record.OccurredAt, location), Type: record.Type,
			Direction: record.Direction, Sentence: EventSentence(record.Type)})
	}
	return result
}

// SyncRow 是同步操作历史的一行。
type SyncRow struct {
	Revision    int64
	ReasonLabel string
	Phase       string
	StateLabel  string
	Attempts    int
	NextAttempt string
	ErrorText   string
	CompletedAt string
}

func NewSyncRows(operations []domain.SynchronizationOperation, location *time.Location) []SyncRow {
	result := make([]SyncRow, 0, len(operations))
	for _, op := range operations {
		row := SyncRow{Revision: int64(op.DesiredRevision), ReasonLabel: ReasonLabel(op.Reason), Phase: string(op.Phase),
			StateLabel: SyncStateLabel(op.State), Attempts: op.AttemptCount, ErrorText: ErrorSentence(op.LastErrorCode),
			CompletedAt: FormatTime(op.CompletedAt, location)}
		if op.State == domain.SyncRetryWait || op.State == domain.SyncPending {
			row.NextAttempt = FormatTimeValue(op.NextAttemptAt, location)
		}
		result = append(result, row)
	}
	return result
}
