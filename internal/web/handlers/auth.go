package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/csrf"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/web/views"
)

// 功能入口：AuthHandler 实现登录/登出的单一可判定结果协议（T148）：
// 登录 = 认证 → 会话显式提交并写 cookie → succeeded 审计；任一步失败则撤回 cookie、撤销会话、记录 failed 并返回 500。
// 登出 = 撤销会话 → succeeded 审计；撤销失败记录 failed 并返回 500。会话在每个请求中至多提交一次（middleware.LoadAndSave）。
type AuthHandler struct {
	Service  *application.AuthService
	Sessions *scs.SessionManager
	Renderer Renderer
	Logger   *slog.Logger
}

func (h *AuthHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.Default()
	}
	return h.Logger
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
	admin, err := h.Service.Authenticate(r.Context(), username, password, source)
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
	// 认证结果协议（T152）：会话行与 succeeded 审计在同一 SQLite 事务提交，成功后才写 cookie；
	// 任一步失败整体回滚，丢弃内存会话，再以独立事务记录 failed 审计并返回 500。
	if err := h.Sessions.RenewToken(r.Context()); err != nil {
		webmiddleware.AbandonSession(r.Context())
		h.loginNotEstablished(w, r, "login failed: session could not be established")
		return
	}
	h.Sessions.Put(r.Context(), webmiddleware.SessionAdministratorID, admin.ID.String())
	h.Sessions.Put(r.Context(), webmiddleware.SessionPasswordVersion, admin.PasswordVersion)
	token, expiry, err := h.Service.EstablishSession(r.Context(), *admin, func(txCtx context.Context) (string, time.Time, error) {
		return h.Sessions.Commit(txCtx)
	})
	if err != nil {
		_ = h.Sessions.Destroy(r.Context())
		webmiddleware.AbandonSession(r.Context())
		h.loginNotEstablished(w, r, "login failed: session could not be established")
		return
	}
	webmiddleware.SessionEstablished(r.Context(), h.Sessions, w, token, expiry)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *AuthHandler) loginNotEstablished(w http.ResponseWriter, r *http.Request, summary string) {
	_ = h.Service.RecordLoginFailure(r.Context(), summary)
	h.Renderer.Error(w, http.StatusInternalServerError, "登录暂时无法完成", NewRequestID())
}

// Logout：succeeded 审计与当前会话撤销在同一事务提交；任一失败整体回滚，原会话保持可用，
// 再以独立事务记录 failed 审计并返回 500（T152）。
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(h.Sessions.GetString(r.Context(), webmiddleware.SessionAdministratorID))
	version := h.Sessions.GetInt64(r.Context(), webmiddleware.SessionPasswordVersion)
	admin := ports.AdministratorRecord{ID: id, PasswordVersion: version}
	if err := h.Service.RevokeSession(r.Context(), admin, func(txCtx context.Context) error {
		return h.Sessions.Destroy(txCtx)
	}); err != nil {
		_ = h.Service.LogoutFailed(r.Context(), admin, "logout failed: session could not be revoked")
		h.Renderer.Error(w, http.StatusInternalServerError, "退出暂时无法完成", NewRequestID())
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
