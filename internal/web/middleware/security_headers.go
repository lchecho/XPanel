package middleware

import "net/http"

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// AI-LOCK: 必须是 same-origin，不能改回 no-referrer。no-referrer 会让浏览器把非 CORS
		// 表单 POST 的 Origin 序列化为 "null"，且完全不发 Referer，于是两道 CSRF 防线同时失效：
		// ① 裸 IP + HTTP 部署下 Fetch Metadata 不会附带 Sec-Fetch-*（规范只对 HTTPS/localhost
		//    这类 potentially trustworthy URL 追加），CrossOriginProtection 只剩 "null" 可比对 → 判为跨站；
		// ② HTTPS 下 gorilla/csrf 对 TLS 请求强制要求同源 Referer，缺失即 ErrNoReferer。
		// same-origin 同样不向第三方泄露来源，但保留同源请求的 Origin 与 Referer。
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
