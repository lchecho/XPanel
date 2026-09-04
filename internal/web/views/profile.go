package views

import (
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// ProfileView 是访问配置页面的展示模型；绝不包含服务端密钥。
type ProfileView struct {
	ID                 string
	Name               string
	InboundTag         string
	PublicHost         string
	PublicPort         int
	Method             string
	Network            string
	BootstrapID        string
	Compatibility      string
	CompatibilityLabel string
	Compatible         bool
	Reason             string
	LastValidatedAt    string
	Revision           int64
	Archived           bool
}

func NewProfileView(record ports.ProfileRecord, location *time.Location) ProfileView {
	p := record.Profile
	return ProfileView{ID: p.ID.String(), Name: p.Name, InboundTag: p.InboundTag, PublicHost: p.PublicHost,
		PublicPort: p.PublicPort, Method: p.Method, Network: string(p.Network), BootstrapID: p.BootstrapStatisticsID,
		Compatibility: string(p.Compatibility), CompatibilityLabel: CompatibilityLabel(p.Compatibility),
		Compatible: p.Compatibility == domain.CompatibilityCompatible, Reason: p.CompatibilityReason,
		LastValidatedAt: FormatTime(p.LastValidatedAt, location), Revision: int64(p.Revision), Archived: p.ArchivedAt != nil}
}

func NewProfileViews(records []ports.ProfileRecord, location *time.Location) []ProfileView {
	result := make([]ProfileView, 0, len(records))
	for _, record := range records {
		result = append(result, NewProfileView(record, location))
	}
	return result
}
