package handlers

import (
	"net/http"

	"xpanel/internal/application"
	"xpanel/internal/domain"
)

// 功能入口：SettingsHandler 提供面板级设置页（全局配额时区）。
type SettingsHandler struct {
	Base
	Service *application.SettingsService
}

type settingsData struct {
	Timezone string
	Revision int64
}

func (h *SettingsHandler) Show(w http.ResponseWriter, r *http.Request) {
	settings, err := h.Service.Get(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	page := h.NewPage(r, "面板设置")
	page.Version = int64(settings.Revision)
	page.Values["quota_timezone"] = settings.QuotaTimezone
	page.Data = settingsData{Timezone: settings.QuotaTimezone, Revision: int64(settings.Revision)}
	h.Renderer.Page(w, http.StatusOK, "settings.html", page)
}

func (h *SettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	form, err := ParseCommandForm(r, h.SessionToken(r), "settings_update", "settings")
	if err != nil || form.Version == nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效或缺少资源版本", "")
		return
	}
	_, err = h.Service.Update(r.Context(), application.UpdateSettingsInput{QuotaTimezone: form.Values["quota_timezone"],
		ExpectedRevision: domain.Revision(*form.Version), RequestID: form.RequestID, ActorID: h.Actor(r), Fingerprint: form.Fingerprint})
	if err != nil {
		failure := Classify(err)
		if failure.Status == http.StatusInternalServerError {
			h.Fail(w, r, err)
			return
		}
		settings, getErr := h.Service.Get(r.Context())
		if getErr != nil {
			h.Fail(w, r, getErr)
			return
		}
		page := h.NewPage(r, "面板设置")
		page.Values = form.Values
		page.ErrorSummary = failure.Message
		if failure.Field != "" {
			page.FieldErrors[failure.Field] = fieldMessage(failure.Field, "时区必须是有效的 IANA 名称，例如 Asia/Shanghai")
		}
		page.Version = int64(settings.Revision)
		page.Data = settingsData{Timezone: settings.QuotaTimezone, Revision: int64(settings.Revision)}
		h.Renderer.Page(w, failure.Status, "settings.html", page)
		return
	}
	h.Flash(r, "success", "配额时区已更新，将从下一个配额周期起生效")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
