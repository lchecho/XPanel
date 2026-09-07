package views

import (
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// TemplateView 是入站模板页面的展示模型；模板不再持有任何密钥。
type TemplateView struct {
	ID                 string
	Name               string
	PublicHost         string
	ListenAddress      string
	PortPoolStart      int
	PortPoolEnd        int
	PortPoolCapacity   int
	Method             string
	Network            string
	Compatibility      string
	CompatibilityLabel string
	Compatible         bool
	Reason             string
	LastValidatedAt    string
	Revision           int64
	Archived           bool
	// PortsAssigned / PortsRemaining / PortsOutside 由端口池占用统计填充，用于容量与池外提示。
	PortsAssigned  int
	PortsRemaining int
	PortsOutside   []int
}

func NewTemplateView(record ports.TemplateRecord, location *time.Location) TemplateView {
	t := record.Template
	return TemplateView{ID: t.ID.String(), Name: t.Name, PublicHost: t.PublicHost, ListenAddress: t.ListenAddress,
		PortPoolStart: t.Pool.Start, PortPoolEnd: t.Pool.End, PortPoolCapacity: t.Pool.Capacity(),
		Method: t.Method, Network: string(t.Network),
		Compatibility: string(t.Compatibility), CompatibilityLabel: CompatibilityLabel(t.Compatibility),
		Compatible: t.Compatibility == domain.CompatibilityCompatible, Reason: t.CompatibilityReason,
		LastValidatedAt: FormatTime(t.LastValidatedAt, location), Revision: int64(t.Revision), Archived: t.ArchivedAt != nil}
}

func NewTemplateViews(records []ports.TemplateRecord, location *time.Location) []TemplateView {
	result := make([]TemplateView, 0, len(records))
	for _, record := range records {
		result = append(result, NewTemplateView(record, location))
	}
	return result
}
