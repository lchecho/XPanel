package views

import (
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// UserView 是用户列表与详情的展示模型；不含任何密钥材料。
type UserView struct {
	ID                 string
	DisplayName        string
	TemplateID         string
	TemplateName       string
	TemplateCompatible bool
	Port               int
	InboundTag         string
	Listening          bool
	TemplateStateLabel string
	State              string
	StateLabel         string
	PendingSync        bool
	SyncError          string
	SyncErrorKind      string
	LastSyncAt         string
	Deleted            bool
	AdminEnabled       bool
	QuotaExceeded      bool
	Revision           int64
	CreatedAt          string
	Quota              QuotaView
}

// QuotaView 按 data-model §QuotaCycle 展示规则计算剩余量与百分比。
type QuotaView struct {
	Unlimited     bool
	LimitBytes    int64
	LimitText     string
	UplinkText    string
	DownlinkText  string
	UsedBytes     int64
	UsedText      string
	RemainingText string
	OverageText   string
	Percent       int
	ResetDay      int
	CycleStart    string
	CycleEnd      string
	Timezone      string
	GrossText     string
}

func NewUserView(record ports.UserRecord, location *time.Location) UserView {
	state := record.Allocation.DisplayState(record.User)
	view := UserView{ID: record.User.ID.String(), DisplayName: record.User.DisplayName,
		TemplateID: record.Template.Template.ID.String(), TemplateName: record.Template.Template.Name,
		TemplateCompatible: record.Template.Template.Compatibility == domain.CompatibilityCompatible,
		TemplateStateLabel: CompatibilityLabel(record.Template.Template.Compatibility),
		Port:               record.Inbound.Inbound.Port, InboundTag: record.Inbound.Inbound.InboundTag,
		Listening: record.Inbound.Inbound.ObservedPresent != nil && *record.Inbound.Inbound.ObservedPresent,
		State:     string(state), StateLabel: StateLabel(state),
		PendingSync:   record.User.Lifecycle != domain.LifecycleDeleted && record.Allocation.PendingSync(),
		SyncErrorKind: record.Allocation.LastSyncErrorCode, SyncError: ErrorSentence(record.Allocation.LastSyncErrorCode),
		LastSyncAt: FormatTime(record.Allocation.LastSyncAt, location), Deleted: record.User.Lifecycle == domain.LifecycleDeleted,
		AdminEnabled: record.Allocation.AdminEnabled, QuotaExceeded: record.Allocation.QuotaState == domain.QuotaExceeded,
		Revision: int64(record.User.Revision), CreatedAt: FormatTimeValue(record.User.CreatedAt, location),
		Quota: NewQuotaView(record.Policy, record.Cycle, location)}
	return view
}

func NewQuotaView(policy ports.QuotaPolicyRecord, cycle ports.QuotaCycleRecord, location *time.Location) QuotaView {
	used := cycle.AccountedUplinkBytes + cycle.AccountedDownlinkBytes
	view := QuotaView{UplinkText: FormatBytes(cycle.AccountedUplinkBytes), DownlinkText: FormatBytes(cycle.AccountedDownlinkBytes),
		UsedBytes: used, UsedText: FormatBytes(used), ResetDay: policy.ResetDay, Timezone: cycle.Timezone,
		CycleStart: FormatTimeValue(cycle.StartsAt, location), CycleEnd: FormatTimeValue(cycle.EndsAt, location),
		GrossText: FormatBytes(cycle.GrossUplinkBytes + cycle.GrossDownlinkBytes)}
	if policy.LimitBytes == nil {
		view.Unlimited = true
		view.LimitText = "无限制"
		view.RemainingText = "无限制"
		return view
	}
	limit := *policy.LimitBytes
	view.LimitBytes = limit
	view.LimitText = FormatBytes(limit)
	remaining := limit - used
	if remaining < 0 {
		view.OverageText = FormatBytes(-remaining)
		remaining = 0
	}
	view.RemainingText = FormatBytes(remaining)
	if limit > 0 {
		view.Percent = int((used * 100) / limit)
	}
	return view
}

func NewUserViews(records []ports.UserRecord, location *time.Location) []UserView {
	result := make([]UserView, 0, len(records))
	for _, record := range records {
		result = append(result, NewUserView(record, location))
	}
	return result
}
