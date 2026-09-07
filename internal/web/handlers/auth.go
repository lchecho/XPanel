package handlers

import (
	"errors"
	"log/slog"
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
	// 认证结果语义：只有会话持久化成功后才记录 succeeded；任一步失败都记录 failed 并拒绝请求，不留下已登录会话（FR-025/FR-027）。
	if err := h.Sessions.RenewToken(r.Context()); err != nil {
		h.loginNotEstablished(w, r, "login failed: session could not be established")
		return
	}
	h.Sessions.Put(r.Context(), webmiddleware.SessionAdministratorID, admin.ID.String())
	h.Sessions.Put(r.Context(), webmiddleware.SessionPasswordVersion, admin.PasswordVersion)
	if err := webmiddleware.EstablishSession(r.Context(), h.Sessions, w); err != nil {
		// 提交失败：数据库中没有会话行，销毁内存会话即可（中间件随后只写过期 cookie）。
		_ = h.Sessions.Destroy(r.Context())
		h.loginNotEstablished(w, r, "login failed: session could not be persisted")
		return
	}
	if err := h.Service.RecordLogin(r.Context(), *admin); err != nil {
		// 补偿：撤回已写入响应的会话 cookie，并撤销已持久化的会话；撤销失败再撤销该管理员全部会话，仍失败则记录错误。
		webmiddleware.WithdrawSessionCookie(w, h.Sessions)
		if destroyErr := h.Sessions.Destroy(r.Context()); destroyErr != nil {
			if revokeErr := h.Service.RevokeSessions(r.Context(), admin.ID); revokeErr != nil {
				h.logger().Error("login compensation failed: persisted session could not be revoked", "error_kind", "internal")
			}
		}
		_ = h.Service.RecordLoginFailure(r.Context(), "login failed: audit could not be written; session revoked")
		h.Renderer.Error(w, http.StatusInternalServerError, "登录暂时无法完成", NewRequestID())
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *AuthHandler) loginNotEstablished(w http.ResponseWriter, r *http.Request, summary string) {
	_ = h.Service.RecordLoginFailure(r.Context(), summary)
	h.Renderer.Error(w, http.StatusInternalServerError, "登录暂时无法完成", NewRequestID())
}

// Logout 先撤销当前会话，确认撤销后才记录 succeeded；撤销失败记录 failed 并返回 500，审计写入失败同样不返回成功。
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	id := domain.ID(h.Sessions.GetString(r.Context(), webmiddleware.SessionAdministratorID))
	version := h.Sessions.GetInt64(r.Context(), webmiddleware.SessionPasswordVersion)
	admin := ports.AdministratorRecord{ID: id, PasswordVersion: version}
	if err := h.Sessions.Destroy(r.Context()); err != nil {
		_ = h.Service.LogoutFailed(r.Context(), admin, "logout failed: session could not be revoked")
		h.Renderer.Error(w, http.StatusInternalServerError, "退出暂时无法完成", NewRequestID())
		return
	}
	if err := h.Service.Logout(r.Context(), admin); err != nil {
		h.Renderer.Error(w, http.StatusInternalServerError, "退出暂时无法完成", NewRequestID())
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
