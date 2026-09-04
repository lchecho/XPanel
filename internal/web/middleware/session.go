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
