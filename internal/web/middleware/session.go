package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"

	"xpanel/internal/domain"
)

const (
	SessionAdministratorID = "administrator_id"
	SessionPasswordVersion = "password_version"
)

type SessionValidator func(context.Context, domain.ID, int64) (bool, error)

func ConfigureSessions(manager *scs.SessionManager, store scs.Store, idle, absolute time.Duration, secure bool) {
	manager.Store = store
	manager.HashTokenInStore = false
	manager.IdleTimeout = idle
	manager.Lifetime = absolute
	manager.Cookie.Name = "__Host-xpanel_session"
	if !secure {
		manager.Cookie.Name = "xpanel_session"
	}
	manager.Cookie.Path = "/"
	manager.Cookie.Domain = ""
	manager.Cookie.HttpOnly = true
	manager.Cookie.Secure = secure
	manager.Cookie.SameSite = http.SameSiteStrictMode
	manager.Cookie.Persist = false
}

func RequireAuth(manager *scs.SessionManager, validate SessionValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := domain.ID(manager.GetString(r.Context(), SessionAdministratorID))
		version := manager.GetInt64(r.Context(), SessionPasswordVersion)
		valid := id.Valid() && version > 0
		if valid && validate != nil {
			var err error
			valid, err = validate(r.Context(), id, version)
			if err != nil {
				http.Error(w, "请求暂时无法完成", http.StatusInternalServerError)
				return
			}
		}
		if !valid {
			if strings.HasPrefix(r.URL.Path, "/fragments/") || r.Header.Get("HX-Request") == "true" {
				http.Error(w, "会话已失效，请重新登录", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sessionCommitState 记录本请求会话的最终处置：已由登录事务建立（不再提交）或已放弃（只写过期 cookie）。
type sessionCommitState struct {
	committed bool
	abandoned bool
}

type sessionStateKey struct{}

// LoadAndSave 是面板自己的会话中间件，区分两类会话写入（T152/T153）：
//   - 安全关键的会话建立/撤销由登录/登出 handler 在与审计同一事务内完成，本中间件不再提交；
//   - 既有会话的辅助刷新（idle 滑动、flash）在首次写响应前提交；失败时保留原会话与 handler 的真实业务响应，
//     不写新 cookie，只记录脱敏诊断——业务事实已由 service 提交，不得因辅助保存失败改报 500。
func LoadAndSave(manager *scs.SessionManager, logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "session")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if cookie, err := r.Cookie(manager.Cookie.Name); err == nil {
			token = cookie.Value
		}
		ctx, err := manager.Load(r.Context(), token)
		if err != nil {
			http.Error(w, "请求暂时无法完成", http.StatusInternalServerError)
			return
		}
		state := &sessionCommitState{}
		ctx = context.WithValue(ctx, sessionStateKey{}, state)
		writer := &sessionWriter{ResponseWriter: w, manager: manager, logger: logger, ctx: ctx, state: state}
		next.ServeHTTP(writer, r.WithContext(ctx))
		writer.finish()
	})
}

type sessionWriter struct {
	http.ResponseWriter
	manager *scs.SessionManager
	logger  *slog.Logger
	ctx     context.Context
	state   *sessionCommitState
	written bool
}

// finish 在首次写响应前恰好执行一次：按会话状态做辅助提交或写过期 cookie；辅助提交失败不改变响应状态。
func (sw *sessionWriter) finish() {
	if sw.written {
		return
	}
	sw.written = true
	if sw.state.abandoned {
		sw.manager.WriteSessionCookie(sw.ctx, sw.ResponseWriter, "", time.Time{})
		return
	}
	switch sw.manager.Status(sw.ctx) {
	case scs.Modified:
		if sw.state.committed {
			return
		}
		token, expiry, err := sw.manager.Commit(sw.ctx)
		if err != nil {
			sw.logger.Warn("auxiliary session refresh failed; existing session kept, response unchanged", "result", "degraded", "error_kind", "session_refresh_failed")
			return
		}
		sw.manager.WriteSessionCookie(sw.ctx, sw.ResponseWriter, token, expiry)
	case scs.Destroyed:
		sw.manager.WriteSessionCookie(sw.ctx, sw.ResponseWriter, "", time.Time{})
	}
}

func (sw *sessionWriter) WriteHeader(code int) {
	sw.finish()
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *sessionWriter) Write(b []byte) (int, error) {
	sw.finish()
	return sw.ResponseWriter.Write(b)
}

// SessionEstablished 由登录 handler 在“会话 + 审计”事务成功后调用：写会话 cookie，并标记本请求不再提交。
func SessionEstablished(ctx context.Context, manager *scs.SessionManager, w http.ResponseWriter, token string, expiry time.Time) {
	manager.WriteSessionCookie(ctx, w, token, expiry)
	if state, ok := ctx.Value(sessionStateKey{}).(*sessionCommitState); ok {
		state.committed = true
	}
}

// AbandonSession 由登录失败路径调用：内存会话不得被中间件提交，响应只写过期 cookie。
func AbandonSession(ctx context.Context) {
	if state, ok := ctx.Value(sessionStateKey{}).(*sessionCommitState); ok {
		state.abandoned = true
	}
}
