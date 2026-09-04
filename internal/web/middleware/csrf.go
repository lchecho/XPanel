package middleware

import (
	"net/http"

	"github.com/gorilla/csrf"
)

func CSRF(authKey []byte, secure bool, next http.Handler) http.Handler {
	cookieName := "__Host-xpanel_csrf"
	if !secure {
		cookieName = "xpanel_csrf"
	}
	protected := csrf.Protect(authKey,
		csrf.FieldName("_csrf"),
		csrf.CookieName(cookieName),
		csrf.Path("/"),
		csrf.Secure(secure),
		csrf.HttpOnly(true),
		csrf.SameSite(csrf.SameSiteStrictMode),
		csrf.ErrorHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "请求完整性校验失败", http.StatusForbidden)
		})),
	)(next)
	if !secure {
		csrfHandler := protected
		protected = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			csrfHandler.ServeHTTP(w, csrf.PlaintextHTTPRequest(r))
		})
	}
	origin := http.NewCrossOriginProtection()
	origin.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "跨站请求已拒绝", http.StatusForbidden)
	}))
	return origin.Handler(protected)
}
