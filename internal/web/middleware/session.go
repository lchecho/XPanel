package middleware

import (
	"context"
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

// sessionCommitState 记录本请求的会话是否已由 handler 显式提交（登录路径），中间件据此不再重复提交。
type sessionCommitState struct {
	committed bool
}

type sessionStateKey struct{}

// LoadAndSave 是面板自己的会话中间件，替代 scs 自带实现以保证“每个请求会话至多提交一次”的可判定协议：
// 登录 handler 通过 EstablishSession 显式提交并写 cookie 后，中间件不再二次提交；其他请求在首次写响应前提交，
// 提交失败返回 500 且不写会话 cookie；已销毁的会话只写过期 cookie（FR-025/FR-027）。
func LoadAndSave(manager *scs.SessionManager, next http.Handler) http.Handler {
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
		writer := &sessionWriter{ResponseWriter: w, manager: manager, ctx: ctx, state: state}
		next.ServeHTTP(writer, r.WithContext(ctx))
		writer.finish()
	})
}

type sessionWriter struct {
	http.ResponseWriter
	manager *scs.SessionManager
	ctx     context.Context
	state   *sessionCommitState
	written bool
	failed  bool
}

// finish 在首次写响应前恰好执行一次：按会话状态提交或写过期 cookie。
func (sw *sessionWriter) finish() {
	if sw.written {
		return
	}
	sw.written = true
	switch sw.manager.Status(sw.ctx) {
	case scs.Modified:
		if sw.state.committed {
			return
		}
		token, expiry, err := sw.manager.Commit(sw.ctx)
		if err != nil {
			sw.failed = true
			http.Error(sw.ResponseWriter, "请求暂时无法完成", http.StatusInternalServerError)
			return
		}
		sw.manager.WriteSessionCookie(sw.ctx, sw.ResponseWriter, token, expiry)
	case scs.Destroyed:
		sw.manager.WriteSessionCookie(sw.ctx, sw.ResponseWriter, "", time.Time{})
	}
}

func (sw *sessionWriter) WriteHeader(code int) {
	sw.finish()
	if sw.failed {
		return
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *sessionWriter) Write(b []byte) (int, error) {
	sw.finish()
	if sw.failed {
		return len(b), nil
	}
	return sw.ResponseWriter.Write(b)
}

// EstablishSession 显式提交当前会话并写会话 cookie；成功后本请求不再由中间件重复提交，登录只在此步成功后才算建立。
func EstablishSession(ctx context.Context, manager *scs.SessionManager, w http.ResponseWriter) error {
	token, expiry, err := manager.Commit(ctx)
	if err != nil {
		return err
	}
	manager.WriteSessionCookie(ctx, w, token, expiry)
	if state, ok := ctx.Value(sessionStateKey{}).(*sessionCommitState); ok {
		state.committed = true
	}
	return nil
}

// WithdrawSessionCookie 从尚未发送的响应中移除会话 cookie（登录补偿路径），保留其他 cookie。
func WithdrawSessionCookie(w http.ResponseWriter, manager *scs.SessionManager) {
	header := w.Header()
	values := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, value := range values {
		if !strings.HasPrefix(value, manager.Cookie.Name+"=") {
			header.Add("Set-Cookie", value)
		}
	}
}
