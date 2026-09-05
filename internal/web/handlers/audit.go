package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/web/views"
)

// 功能入口：AuditHandler 渲染审计列表（筛选 + 游标分页）；绝不展示密码、密钥、令牌或完整连接 URI。
type AuditHandler struct {
	Base
	Service *application.AuditService
	Users   *application.UserService
}

type auditData struct {
	Events     []views.AuditRow
	Users      []views.UserView
	Actions    []views.Option
	Results    []views.Option
	TargetID   string
	Action     string
	Result     string
	NextCursor string
	Filtered   bool
}

const auditPageSize = 50

func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := ports.AuditFilter{Action: query.Get("action"), Result: query.Get("result"), Limit: auditPageSize}
	if target := domain.ID(query.Get("user")); target.Valid() {
		filter.TargetID = target
	}
	if !views.ValidAuditAction(filter.Action) {
		filter.Action = ""
	}
	if !views.ValidAuditResult(filter.Result) {
		filter.Result = ""
	}
	if cursor, ok := parseAuditCursor(query.Get("cursor")); ok {
		filter.Before = &cursor
	}
	events, next, err := h.Service.List(r.Context(), filter)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	users, err := h.Users.List(r.Context(), ports.UserFilter{IncludeDeleted: true})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	location := h.Location(r)
	page := h.NewPage(r, "审计记录")
	data := auditData{Events: views.NewAuditRows(events, users, location), Users: views.NewUserViews(users, location),
		Actions: views.AuditActionOptions(), Results: views.AuditResultOptions(), TargetID: filter.TargetID.String(),
		Action: filter.Action, Result: filter.Result, Filtered: filter.TargetID != "" || filter.Action != "" || filter.Result != ""}
	if next != nil {
		values := url.Values{}
		if filter.TargetID != "" {
			values.Set("user", filter.TargetID.String())
		}
		if filter.Action != "" {
			values.Set("action", filter.Action)
		}
		if filter.Result != "" {
			values.Set("result", filter.Result)
		}
		values.Set("cursor", strconv.FormatInt(next.OccurredAt.UnixMilli(), 10)+":"+next.ID.String())
		data.NextCursor = "/audit?" + values.Encode()
	}
	page.Data = data
	h.Renderer.Page(w, http.StatusOK, "audit.html", page)
}

func parseAuditCursor(raw string) (ports.AuditCursor, bool) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return ports.AuditCursor{}, false
	}
	millis, err := strconv.ParseInt(parts[0], 10, 64)
	id := domain.ID(parts[1])
	if err != nil || !id.Valid() {
		return ports.AuditCursor{}, false
	}
	return ports.AuditCursor{OccurredAt: time.UnixMilli(millis).UTC(), ID: id}, true
}
