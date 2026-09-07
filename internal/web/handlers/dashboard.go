package handlers

import (
	"net/http"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/ports"
	"xpanel/internal/web/views"
)

// 功能入口：DashboardHandler 渲染仪表盘整页；数据只读自 SQLite（FR-024）。
type DashboardHandler struct {
	Base
	Dashboard *application.DashboardService
}

func (h *DashboardHandler) Show(w http.ResponseWriter, r *http.Request) {
	summary, err := h.Dashboard.Summary(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "仪表盘")
	page.Data = NewDashboardView(summary, h.Location(r))
	h.Renderer.Page(w, http.StatusOK, "dashboard.html", page)
}

// NewDashboardView 把服务层汇总映射为展示模型。
func NewDashboardView(summary application.DashboardSummary, location *time.Location) views.DashboardView {
	instance := summary.Instance
	return views.DashboardView{HealthState: instance.HealthState, HealthLabel: views.HealthLabel(instance.HealthState),
		LastSuccessAt: views.FormatTime(instance.LastSuccessAt, location), LastErrorText: views.ErrorSentence(instance.LastErrorCode),
		LastCollectionAt: views.FormatTime(summary.LastCollectionAt, location), Stale: summary.Stale,
		GeneratedAt: views.FormatTimeValue(summary.GeneratedAt, location), TotalUsers: summary.TotalUsers, Active: summary.Active,
		Disabled: summary.Disabled, QuotaExceeded: summary.QuotaExceeded, Pending: summary.Pending, StuckSync: summary.StuckSync, Deleted: summary.Deleted,
		AccountedText: views.FormatBytes(summary.AccountedBytes), PortsCapacity: summary.PortsCapacity, PortsAssigned: summary.PortsAssigned,
		PortsRemaining: summary.PortsRemaining, PortsOutside: summary.PortsOutside, PortsRebuilding: summary.PortsRebuilding,
		Failed: views.NewFailedOperationViews(summary.Failed, location)}
}

// FragmentHandler 提供 HTMX 轮询的只读 fragment；不含布局、不调用 Xray。
type FragmentHandler struct {
	Base
	Dashboard *application.DashboardService
	Users     *application.UserService
}

func (h *FragmentHandler) Summary(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "HX-Request")
	summary, err := h.Dashboard.Summary(r.Context())
	if err != nil {
		h.Renderer.Fragment(w, http.StatusServiceUnavailable, "fragment_unavailable.html", views.Page{ErrorSummary: "数据暂时不可用，页面显示的是最后一次确认的数据（陈旧）"})
		return
	}
	page := views.Page{Authenticated: true, Timezone: h.Location(r).String(), Data: NewDashboardView(summary, h.Location(r))}
	h.Renderer.Fragment(w, http.StatusOK, "dashboard_summary.html", page)
}

func (h *FragmentHandler) UsersTable(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "HX-Request")
	query := r.URL.Query().Get("q")
	status := r.URL.Query().Get("status")
	if !validStatus(status) {
		status = ""
	}
	includeDeleted := r.URL.Query().Get("deleted") == "1"
	records, err := h.Users.List(r.Context(), ports.UserFilter{Query: query, Status: status, IncludeDeleted: includeDeleted})
	if err != nil {
		h.Renderer.Fragment(w, http.StatusServiceUnavailable, "fragment_unavailable.html", views.Page{ErrorSummary: "用户列表暂时不可用，显示的是最后一次确认的数据（陈旧）"})
		return
	}
	summary, err := h.Dashboard.Summary(r.Context())
	stale := err != nil || summary.Stale
	page := views.Page{Authenticated: true, Timezone: h.Location(r).String(), Data: userListData{Users: views.NewUserViews(records, h.Location(r)),
		Query: query, Status: status, Statuses: statusOptions, Total: len(records), IncludeDeleted: includeDeleted, Stale: stale,
		LastCollectionAt: views.FormatTime(summary.LastCollectionAt, h.Location(r))}}
	h.Renderer.Fragment(w, http.StatusOK, "users_table.html", page)
}
