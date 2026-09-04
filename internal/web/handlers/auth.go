package handlers

import (
	"errors"
	"net"
	"net/http"
	"strconv"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/csrf"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/web/views"
)

type AuthHandler struct {
	Service  *application.AuthService
	Sessions *scs.SessionManager
	Renderer Renderer
}

func (h *AuthHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if h.Sessions.GetString(r.Context(), webmiddleware.SessionAdministratorID) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.Renderer.Page(w, http.StatusOK, "login.html", views.Page{Title: "登录", CSRFField: csrf.TemplateField(r), RequestID: NewRequestID()})
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.Renderer.Error(w, http.StatusBadRequest, "表单格式无效", "")
		return
	}
	username := r.PostForm.Get("username")
	password := []byte(r.PostForm.Get("password"))
	source, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		source = r.RemoteAddr
	}
	admin, err := h.Service.Login(r.Context(), username, password, source)
	if err != nil {
		var limited *domain.RateLimitError
		if errors.As(err, &limited) {
			seconds := int(limited.RetryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			h.Renderer.Page(w, http.StatusTooManyRequests, "login.html", views.Page{Title: "登录", CSRFField: csrf.TemplateField(r),
				RequestID: NewRequestID(), ErrorSummary: "尝试次数过多，请稍后重试", Values: map[string]string{"username": username}})
			return
		}
		if errors.Is(err, application.ErrInvalidCredentials) {
			h.Renderer.Page(w, http.StatusUnauthorized, "login.html", views.Page{Title: "登录", CSRFField: csrf.TemplateField(r),
				RequestID: NewRequestID(), ErrorSummary: "用户名或密码错误", Values: map[string]string{"username": username}})
			return
		}
		h.Renderer.Error(w, http.StatusInternalServerError, "登录暂时无法完成", NewRequestID())
		return
	}
	if err := h.Sessions.RenewToken(r.Context()); err != nil {
		h.Renderer.Error(w, http.StatusInternalServerError, "登录暂时无法完成", NewRequestID())
		return
	}
	h.Sessions.Put(r.Context(), webmiddleware.SessionAdministratorID, admin.ID.String())
	h.Sessions.Put(r.Context(), webmiddleware.SessionPasswordVersion, admin.PasswordVersion)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(h.Sessions.GetString(r.Context(), webmiddleware.SessionAdministratorID))
	version := h.Sessions.GetInt64(r.Context(), webmiddleware.SessionPasswordVersion)
	_ = h.Service.Logout(r.Context(), ports.AdministratorRecord{ID: id, PasswordVersion: version})
	if err := h.Sessions.Destroy(r.Context()); err != nil {
		h.Renderer.Error(w, http.StatusInternalServerError, "退出暂时无法完成", NewRequestID())
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
