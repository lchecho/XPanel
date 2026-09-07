package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/web/views"
)

// 功能入口：UserHandler 提供受管用户的列表、创建、详情与连接信息页面。
// 职责：解析表单并调用 UserService/ConnectionService；不负责生成密钥或访问 Xray。
// 约束：连接信息只在凭证被 Xray 确认后展示，且页面 no-store；密钥不作为独立字段记录。
type UserHandler struct {
	Base
	Service     *application.UserService
	Templates   *application.TemplateService
	Connections *application.ConnectionService
	Dashboard   *application.DashboardService
}

const activeCapacity = 20

var quotaUnits = []string{"MiB", "GiB", "TiB"}

type userFormData struct {
	Templates    []views.TemplateView
	NoCompatible bool
	Units        []string
}

type userListData struct {
	Users            []views.UserView
	Query            string
	Status           string
	Statuses         []statusOption
	Total            int
	IncludeDeleted   bool
	Stale            bool
	LastCollectionAt string
}

// FragmentQuery 生成 users-table fragment 的轮询地址，保留当前筛选条件。
func (d userListData) FragmentQuery() string {
	values := url.Values{}
	if d.Query != "" {
		values.Set("q", d.Query)
	}
	if d.Status != "" {
		values.Set("status", d.Status)
	}
	if d.IncludeDeleted {
		values.Set("deleted", "1")
	}
	if len(values) == 0 {
		return "/fragments/users-table"
	}
	return "/fragments/users-table?" + values.Encode()
}

type statusOption struct{ Value, Label string }

var statusOptions = []statusOption{{"", "全部（不含已删除）"}, {"active", "已启用"}, {"disabled", "手动禁用"},
	{"quota_exceeded", "配额超限"}, {"pending", "待同步"}, {"deleted", "已删除"}}

type userDetailData struct {
	User                views.UserView
	ConnectionAvailable bool
	ConnectionPending   bool
	Daily               []views.DailyRow
	Events              []views.EventRow
	Sync                []views.SyncRow
	NodeHealth          string
	NodeError           string
	NodeLastSuccess     string
}

type connectionData struct {
	User     views.UserView
	Pending  bool
	Inactive bool
	Host     string
	Port     int
	Method   string
	Label    string
	Password string
	URI      string
}

func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	if !validStatus(status) {
		status = ""
	}
	includeDeleted := r.URL.Query().Get("deleted") == "1"
	records, err := h.Service.List(r.Context(), ports.UserFilter{Query: query, Status: status, IncludeDeleted: includeDeleted})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "用户")
	page.Capacity = h.capacityNotice(r)
	page.Data = userListData{Users: views.NewUserViews(records, h.Location(r)), Query: query, Status: status,
		Statuses: statusOptions, Total: len(records), IncludeDeleted: includeDeleted}
	h.Renderer.Page(w, http.StatusOK, "users_list.html", page)
}

func validStatus(status string) bool {
	for _, option := range statusOptions {
		if option.Value == status {
			return true
		}
	}
	return false
}

func (h *UserHandler) NewForm(w http.ResponseWriter, r *http.Request) {
	page := h.NewPage(r, "创建用户")
	page.Values["reset_day"] = "1"
	page.Values["quota_unit"] = "GiB"
	data, err := h.formData(r)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page.Capacity = h.capacityNotice(r)
	page.Data = data
	h.Renderer.Page(w, http.StatusOK, "user_form.html", page)
}

func (h *UserHandler) formData(r *http.Request) (userFormData, error) {
	records, err := h.Templates.List(r.Context(), true)
	if err != nil {
		return userFormData{}, err
	}
	// 端口池占用一并带出，供表单展示剩余可分配数量与耗尽提示（FR-009/FR-035）。
	views := views.NewTemplateViews(records, h.Location(r))
	for i, record := range records {
		if usage, err := h.Templates.PortUsage(r.Context(), record.Template.ID); err == nil {
			views[i].PortsAssigned, views[i].PortsRemaining, views[i].PortsOutside = usage.Assigned, usage.Remaining, usage.Outside
		}
	}
	return userFormData{Templates: views, NoCompatible: len(records) == 0, Units: quotaUnits}, nil
}

func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	form, err := ParseCommandForm(r, h.SessionToken(r), "user_create", "")
	if err != nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效", "")
		return
	}
	input := application.CreateUserInput{DisplayName: form.Values["display_name"], TemplateID: domain.ID(form.Values["template_id"]),
		RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint}
	if !input.TemplateID.Valid() {
		h.renderUserForm(w, r, form.Values, &domain.ValidationError{Field: "template_id", Message: "inbound template is required"})
		return
	}
	// 端口留空表示由面板自动分配；填写时必须是池内未占用端口（FR-007）。
	if raw := strings.TrimSpace(form.Values["port"]); raw != "" {
		port, convErr := strconv.Atoi(raw)
		if convErr != nil {
			h.renderUserForm(w, r, form.Values, &domain.ValidationError{Field: "port", Message: "port must be a number"})
			return
		}
		input.Port = &port
	}
	input.ResetDay, _ = strconv.Atoi(strings.TrimSpace(form.Values["reset_day"]))
	limit, err := parseQuota(form.Values)
	if err != nil {
		h.renderUserForm(w, r, form.Values, err)
		return
	}
	input.LimitBytes = limit
	id, _, err := h.Service.CreateUser(r.Context(), input)
	if err != nil {
		h.renderUserForm(w, r, form.Values, err)
		return
	}
	h.Flash(r, "success", "用户已创建，正在同步到节点")
	http.Redirect(w, r, "/users/"+id.String(), http.StatusSeeOther)
}

// parseQuota 把“整数 + 单位”或“无限制”转换为字节；空值未勾选、零/负值与溢出均为字段错误。
func parseQuota(values map[string]string) (*int64, error) {
	if values["unlimited"] == "on" || values["unlimited"] == "1" {
		return nil, nil
	}
	raw := strings.TrimSpace(values["quota_value"])
	if raw == "" {
		return nil, &domain.ValidationError{Field: "quota", Message: "quota is required unless unlimited is selected"}
	}
	amount, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || amount <= 0 {
		return nil, &domain.ValidationError{Field: "quota", Message: "quota must be a positive integer"}
	}
	var multiplier int64
	switch values["quota_unit"] {
	case "MiB":
		multiplier = 1 << 20
	case "GiB":
		multiplier = 1 << 30
	case "TiB":
		multiplier = 1 << 40
	default:
		return nil, &domain.ValidationError{Field: "quota", Message: "quota unit is invalid"}
	}
	if amount > (1<<62)/multiplier {
		return nil, &domain.ValidationError{Field: "quota", Message: "quota exceeds the supported maximum"}
	}
	bytes := amount * multiplier
	return &bytes, nil
}

func (h *UserHandler) renderUserForm(w http.ResponseWriter, r *http.Request, values map[string]string, cause error) {
	failure := Classify(cause)
	if failure.Status == http.StatusNotFound || failure.Status == http.StatusInternalServerError {
		h.Fail(w, r, cause)
		return
	}
	page := h.NewPage(r, "创建用户")
	page.Values = values
	page.ErrorSummary = failure.Message
	if failure.Field != "" {
		page.FieldErrors[failure.Field] = failure.Message
	}
	data, err := h.formData(r)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page.Capacity = h.capacityNotice(r)
	page.Data = data
	h.Renderer.Page(w, failure.Status, "user_form.html", page)
}

func (h *UserHandler) Detail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "用户："+record.User.DisplayName)
	page.Version = int64(record.User.Revision)
	data := h.detailData(r, record)
	page.Data = data
	h.Renderer.Page(w, http.StatusOK, "user_detail.html", page)
}

// detailData 组装详情页数据：派生状态、连接信息可用性、当前周期日趋势、连续性事件与同步历史。
func (h *UserHandler) detailData(r *http.Request, record ports.UserRecord) userDetailData {
	confirmed := record.Credential.State == domain.CredentialActive && record.Allocation.DesiredCredentialVersion == record.Credential.Version
	data := userDetailData{User: views.NewUserView(record, h.Location(r)),
		ConnectionAvailable: confirmed && record.User.Lifecycle != domain.LifecycleDeleted,
		ConnectionPending:   !confirmed && record.User.Lifecycle != domain.LifecycleDeleted}
	if h.Dashboard == nil {
		return data
	}
	if detail, err := h.Dashboard.UserDetail(r.Context(), record); err == nil {
		data.Daily = views.NewDailyRows(detail.Daily)
		data.Events = views.NewEventRows(detail.Events, h.Location(r))
		data.Sync = views.NewSyncRows(detail.Sync, h.Location(r))
	}
	if summary, err := h.Dashboard.Summary(r.Context()); err == nil {
		data.NodeHealth = views.HealthLabel(summary.Instance.HealthState)
		data.NodeError = views.ErrorSentence(summary.Instance.LastErrorCode)
		data.NodeLastSuccess = views.FormatTime(summary.Instance.LastSuccessAt, h.Location(r))
	}
	return data
}

func (h *UserHandler) Connection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "连接信息："+record.User.DisplayName)
	data := connectionData{User: views.NewUserView(record, h.Location(r))}
	info, err := h.Connections.BuildConnectionInfo(r.Context(), id)
	switch {
	case errors.Is(err, application.ErrConnectionPending):
		data.Pending = true
	case err != nil:
		h.Fail(w, r, err)
		return
	default:
		data.Host, data.Port, data.Method, data.Label = info.Host, info.Port, info.Method, info.Label
		data.Password, data.URI, data.Inactive = info.Password.Reveal(), info.URI.Reveal(), info.Inactive
	}
	page.Data = data
	h.Renderer.Page(w, http.StatusOK, "user_connection.html", page)
}

// capacityNotice 在活跃分配超过验收容量时返回提示文案（spec Assumptions）。
func (h *UserHandler) capacityNotice(r *http.Request) string {
	records, err := h.Service.List(r.Context(), ports.UserFilter{})
	if err != nil || len(records) <= activeCapacity {
		return ""
	}
	return "活跃访问分配已超过 " + strconv.Itoa(activeCapacity) + " 个的验收容量，性能目标不再承诺"
}

type userEditData struct {
	User  views.UserView
	Units []string
}

// EditForm 渲染编辑页：显示名称、配额、重置日与启用意图（http.md §Managed users）。
func (h *UserHandler) EditForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if record.User.Lifecycle == domain.LifecycleDeleted {
		h.Renderer.Error(w, http.StatusConflict, "已删除的用户不能再编辑", "")
		return
	}
	page := h.NewPage(r, "编辑用户："+record.User.DisplayName)
	page.Version = int64(record.User.Revision)
	page.Values = editValues(record)
	page.Data = userEditData{User: views.NewUserView(record, h.Location(r)), Units: quotaUnits}
	h.Renderer.Page(w, http.StatusOK, "user_edit.html", page)
}

func editValues(record ports.UserRecord) map[string]string {
	values := map[string]string{"display_name": record.User.DisplayName, "reset_day": strconv.Itoa(record.Policy.ResetDay), "quota_unit": "GiB"}
	if record.Allocation.AdminEnabled {
		values["admin_enabled"] = "on"
	}
	if record.Policy.LimitBytes == nil {
		values["unlimited"] = "on"
		return values
	}
	limit := *record.Policy.LimitBytes
	for _, unit := range []struct {
		name string
		size int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}} {
		if limit%unit.size == 0 {
			values["quota_value"], values["quota_unit"] = strconv.FormatInt(limit/unit.size, 10), unit.name
			return values
		}
	}
	values["quota_value"], values["quota_unit"] = strconv.FormatInt((limit+(1<<20)-1)/(1<<20), 10), "MiB"
	return values
}

// Update 处理编辑提交：`_version` 保护并发编辑，409 时回填当前值与新版本。
func (h *UserHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), "user_update", id.String())
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	limit, err := parseQuota(form.Values)
	if err != nil {
		h.renderEditForm(w, r, id, form.Values, err)
		return
	}
	resetDay, _ := strconv.Atoi(strings.TrimSpace(form.Values["reset_day"]))
	_, err = h.Service.UpdateUser(r.Context(), application.UpdateUserInput{ID: id, DisplayName: form.Values["display_name"], LimitBytes: limit,
		ResetDay: resetDay, AdminEnabled: form.Values["admin_enabled"] == "on", ExpectedRevision: domain.Revision(*form.Version),
		RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint})
	if err != nil {
		h.renderEditForm(w, r, id, form.Values, err)
		return
	}
	h.Flash(r, "success", "用户已更新；影响节点的变更会自动同步")
	http.Redirect(w, r, "/users/"+id.String(), http.StatusSeeOther)
}

func (h *UserHandler) renderEditForm(w http.ResponseWriter, r *http.Request, id domain.ID, values map[string]string, cause error) {
	failure := Classify(cause)
	if failure.Status == http.StatusNotFound || failure.Status == http.StatusInternalServerError {
		h.Fail(w, r, cause)
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "编辑用户："+record.User.DisplayName)
	page.Values = values
	page.ErrorSummary = failure.Message
	if failure.Field != "" {
		page.FieldErrors[failure.Field] = failure.Message
	}
	page.Version = int64(record.User.Revision)
	page.Data = userEditData{User: views.NewUserView(record, h.Location(r)), Units: quotaUnits}
	h.Renderer.Page(w, failure.Status, "user_edit.html", page)
}

// ResetForm 渲染手动重置确认页，说明只清零 accounted、保留 gross/lifetime/日趋势与周期边界。
func (h *UserHandler) ResetForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "重置本周期流量："+record.User.DisplayName)
	page.Data = views.NewUserView(record, h.Location(r))
	h.Renderer.Page(w, http.StatusOK, "user_reset_traffic.html", page)
}

func (h *UserHandler) Reset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), "user_reset_traffic", id.String())
	if err != nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效", "")
		return
	}
	if _, err := h.Service.ResetTraffic(r.Context(), application.ResetTrafficInput{ID: id, RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint}); err != nil {
		h.Fail(w, r, err)
		return
	}
	h.Flash(r, "success", "本周期用量已清零；若配额是唯一阻断原因，访问将自动恢复")
	http.Redirect(w, r, "/users/"+id.String(), http.StatusSeeOther)
}

// Enable / Disable 处理详情页的启停按钮（POST + 303）。
func (h *UserHandler) Enable(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }
func (h *UserHandler) Disable(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }

func (h *UserHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	action := "user_disable"
	if enabled {
		action = "user_enable"
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), action, id.String())
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	if _, err := h.Service.SetAdminEnabled(r.Context(), application.SetEnabledInput{ID: id, Enabled: enabled,
		ExpectedRevision: domain.Revision(*form.Version), RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint}); err != nil {
		h.conflictOrFail(w, r, id, err)
		return
	}
	if enabled {
		h.Flash(r, "success", "已请求启用；若用量仍符合配额，节点确认后即可建立新连接")
	} else {
		h.Flash(r, "success", "已请求禁用；节点确认后拒绝新连接，已建立的连接可能继续")
	}
	http.Redirect(w, r, "/users/"+id.String(), http.StatusSeeOther)
}

// conflictOrFail 把 409 呈现为带最新状态的详情页（http.md §Response Semantics）。
func (h *UserHandler) conflictOrFail(w http.ResponseWriter, r *http.Request, id domain.ID, cause error) {
	failure := Classify(cause)
	if failure.Status != http.StatusConflict {
		h.Fail(w, r, cause)
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "用户："+record.User.DisplayName)
	page.ErrorSummary = failure.Message
	page.Version = int64(record.User.Revision)
	page.Data = h.detailData(r, record)
	h.Renderer.Page(w, http.StatusConflict, "user_detail.html", page)
}

func (h *UserHandler) RotateForm(w http.ResponseWriter, r *http.Request) {
	h.confirmationPage(w, r, "user_rotate.html", "轮换凭证：")
}

func (h *UserHandler) DeleteForm(w http.ResponseWriter, r *http.Request) {
	h.confirmationPage(w, r, "user_delete.html", "删除用户：")
}

func (h *UserHandler) confirmationPage(w http.ResponseWriter, r *http.Request, template, titlePrefix string) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.User(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if record.User.Lifecycle == domain.LifecycleDeleted {
		h.Renderer.Error(w, http.StatusConflict, "已删除的用户不能再执行此操作", "")
		return
	}
	page := h.NewPage(r, titlePrefix+record.User.DisplayName)
	page.Version = int64(record.User.Revision)
	page.Data = views.NewUserView(record, h.Location(r))
	h.Renderer.Page(w, http.StatusOK, template, page)
}

func (h *UserHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), "user_rotate", id.String())
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	if _, err := h.Service.RotateCredential(r.Context(), application.LifecycleInput{ID: id, ExpectedRevision: domain.Revision(*form.Version),
		RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint}); err != nil {
		h.conflictOrFail(w, r, id, err)
		return
	}
	h.Flash(r, "success", "凭证轮换已开始；节点确认新凭证后，连接信息页才会展示新密码，旧凭证随后失效")
	http.Redirect(w, r, "/users/"+id.String(), http.StatusSeeOther)
}

func (h *UserHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "user_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), "user_delete", id.String())
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	if _, err := h.Service.DeleteUser(r.Context(), application.LifecycleInput{ID: id, ExpectedRevision: domain.Revision(*form.Version),
		RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint}); err != nil {
		h.conflictOrFail(w, r, id, err)
		return
	}
	h.Flash(r, "success", "用户已删除；节点确认移除后凭证销毁，历史用量与审计保留")
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}
