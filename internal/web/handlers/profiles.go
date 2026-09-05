package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/security"
	"xpanel/internal/web/views"
)

// 功能入口：ProfileHandler 提供访问配置的登记、查看、编辑与重新验证页面。
// 职责：只解析表单并调用 ProfileService；不负责验证 Xray、不负责持久化。
// 约束：服务端密钥永不回显；所有状态变更为 POST + 303。
type ProfileHandler struct {
	Base
	Service *application.ProfileService
}

type profileFormData struct {
	Edit     bool
	Profile  views.ProfileView
	Methods  []string
	Networks []string
}

var (
	profileMethods  = []string{security.MethodAES128, security.MethodAES256}
	profileNetworks = []string{string(domain.NetworkTCPUDP), string(domain.NetworkTCP), string(domain.NetworkUDP)}
)

func (h *ProfileHandler) List(w http.ResponseWriter, r *http.Request) {
	records, err := h.Service.List(r.Context(), false)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "访问配置")
	page.Data = views.NewProfileViews(records, h.Location(r))
	h.Renderer.Page(w, http.StatusOK, "profiles_list.html", page)
}

func (h *ProfileHandler) NewForm(w http.ResponseWriter, r *http.Request) {
	page := h.NewPage(r, "登记访问配置")
	page.Values["method"] = security.MethodAES256
	page.Values["network"] = string(domain.NetworkTCPUDP)
	page.Data = profileFormData{Methods: profileMethods, Networks: profileNetworks}
	h.Renderer.Page(w, http.StatusOK, "profile_form.html", page)
}

func (h *ProfileHandler) Create(w http.ResponseWriter, r *http.Request) {
	form, err := ParseCommandForm(r, h.SessionToken(r), "profile_register", "")
	if err != nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效", "")
		return
	}
	input, values := profileInput(r, form)
	input.ActorID = h.Actor(r)
	id, err := h.Service.RegisterProfile(r.Context(), input)
	if err != nil {
		h.renderProfileForm(w, r, false, "", values, err)
		return
	}
	h.Flash(r, "success", "访问配置已保存，正在验证与节点的兼容性")
	http.Redirect(w, r, "/profiles/"+id.String(), http.StatusSeeOther)
}

func (h *ProfileHandler) Detail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "profile_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.Get(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "访问配置："+record.Profile.Name)
	page.Version = int64(record.Profile.Revision)
	page.Data = views.NewProfileView(record, h.Location(r))
	h.Renderer.Page(w, http.StatusOK, "profile_detail.html", page)
}

func (h *ProfileHandler) EditForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "profile_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	record, err := h.Service.Get(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "编辑访问配置")
	page.Version = int64(record.Profile.Revision)
	page.Values = profileValues(record.Profile)
	page.Data = profileFormData{Edit: true, Profile: views.NewProfileView(record, h.Location(r)), Methods: profileMethods, Networks: profileNetworks}
	h.Renderer.Page(w, http.StatusOK, "profile_form.html", page)
}

func (h *ProfileHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "profile_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), "profile_update", id.String())
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	input, values := profileInput(r, form)
	input.ActorID = h.Actor(r)
	input.ExpectedRevision = domain.Revision(*form.Version)
	if err := h.Service.UpdateProfile(r.Context(), id, input); err != nil {
		h.renderProfileForm(w, r, true, id, values, err)
		return
	}
	h.Flash(r, "success", "访问配置已更新")
	http.Redirect(w, r, "/profiles/"+id.String(), http.StatusSeeOther)
}

func (h *ProfileHandler) Revalidate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "profile_id")
	if !ok {
		h.Renderer.Error(w, http.StatusNotFound, "请求的资源不存在", "")
		return
	}
	form, err := ParseCommandForm(r, h.SessionToken(r), "profile_revalidate", id.String())
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	if _, err := h.Service.Revalidate(r.Context(), application.RevalidateInput{ID: id, ExpectedRevision: domain.Revision(*form.Version),
		RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint}); err != nil {
		h.renderProfileForm(w, r, true, id, profileValues(domain.AccessProfile{}), err)
		return
	}
	h.Flash(r, "success", "已重新排队验证访问配置")
	http.Redirect(w, r, "/profiles/"+id.String(), http.StatusSeeOther)
}

func (h *ProfileHandler) renderProfileForm(w http.ResponseWriter, r *http.Request, edit bool, id domain.ID, values map[string]string, cause error) {
	failure := Classify(cause)
	if failure.Status == http.StatusNotFound || failure.Status == http.StatusInternalServerError {
		h.Fail(w, r, cause)
		return
	}
	title := "登记访问配置"
	if edit {
		title = "编辑访问配置"
	}
	page := h.NewPage(r, title)
	page.Values = values
	page.ErrorSummary = failure.Message
	if failure.Field != "" {
		page.FieldErrors[failure.Field] = failure.Message
	}
	data := profileFormData{Edit: edit, Methods: profileMethods, Networks: profileNetworks}
	if edit {
		if record, err := h.Service.Get(r.Context(), id); err == nil {
			// 409 时同时提供当前存储值与新版本号，管理员无需重新填写（http.md §Response Semantics）。
			page.Version = int64(record.Profile.Revision)
			data.Profile = views.NewProfileView(record, h.Location(r))
		}
	}
	page.Data = data
	h.Renderer.Page(w, failure.Status, "profile_form.html", page)
}

func profileInput(r *http.Request, form CommandForm) (application.ProfileInput, map[string]string) {
	values := form.Values
	port, _ := strconv.Atoi(strings.TrimSpace(values["public_port"]))
	input := application.ProfileInput{Name: values["name"], InboundTag: values["inbound_tag"], PublicHost: strings.TrimSpace(values["public_host"]),
		PublicPort: port, Method: values["method"], Network: domain.Network(values["network"]),
		ServerKey: strings.TrimSpace(r.PostForm.Get("server_key")), BootstrapStatisticsID: strings.TrimSpace(values["bootstrap_statistics_id"]),
		RequestID: form.RequestID, Fingerprint: form.Fingerprint}
	return input, values
}

func profileValues(p domain.AccessProfile) map[string]string {
	return map[string]string{"name": p.Name, "inbound_tag": p.InboundTag, "public_host": p.PublicHost,
		"public_port": strconv.Itoa(p.PublicPort), "method": p.Method, "network": string(p.Network),
		"bootstrap_statistics_id": p.BootstrapStatisticsID}
}
