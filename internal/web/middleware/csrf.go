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
	return origin.Handler(normalizeSameOriginNullOrigin(secure, protected))
}

// normalizeSameOriginNullOrigin reconciles Referrer-Policy: no-referrer with
// gorilla/csrf's Origin validation. The Fetch standard serializes Origin as
// "null" for non-CORS POST requests under that policy, including normal
// same-origin HTML form submissions. Sec-Fetch-Site is a forbidden browser
// header, so after CrossOriginProtection has accepted an explicit
// "same-origin" value we can safely restore the effective origin for the
// double-submit token check. Cross-site and unverifiable null origins are left
// untouched and remain rejected.
func normalizeSameOriginNullOrigin(secure bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "null" && r.Header.Get("Sec-Fetch-Site") == "same-origin" {
			clone := r.Clone(r.Context())
			clone.Header = r.Header.Clone()
			scheme := "https"
			if !secure {
				scheme = "http"
			}
			clone.Header.Set("Origin", scheme+"://"+r.Host)
			r = clone
		}
		next.ServeHTTP(w, r)
	})
}
