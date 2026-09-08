package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const crossSiteDenied = "跨站请求已拒绝"

// postThroughCSRF 返回 CSRF 中间件对一次表单 POST 的响应体（去掉换行）。
// 只关心是否被 CrossOriginProtection 判为跨站；令牌校验失败是另一条消息。
func postThroughCSRF(t *testing.T, headers map[string]string) string {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CSRF(make([]byte, 32), false, next)

	r := httptest.NewRequest(http.MethodPost, "http://203.0.113.10:8080/login", strings.NewReader("_csrf=x"))
	r.Host = "203.0.113.10:8080"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return strings.TrimSpace(w.Body.String())
}

// 回归：裸 IP + HTTP 部署。Fetch Metadata 规范只对 potentially trustworthy URL
// （HTTPS 或 localhost）追加 Sec-Fetch-*，所以这里一个 Sec-Fetch 头都没有。
// 只要 Referrer-Policy 让浏览器发出真实 Origin，就不能被判为跨站。
func TestSameOriginFormPostOverPlainHTTPIsAccepted(t *testing.T) {
	body := postThroughCSRF(t, map[string]string{"Origin": "http://203.0.113.10:8080"})
	if body == crossSiteDenied {
		t.Fatalf("同源表单 POST 被判为跨站：%s", body)
	}
}

// Origin 为 "null" 且没有任何 Sec-Fetch-* 佐证时无法证明同源，必须继续拒绝。
// 这正是 Referrer-Policy: no-referrer 曾经造成的状态。
func TestUnverifiableNullOriginIsStillRejected(t *testing.T) {
	if body := postThroughCSRF(t, map[string]string{"Origin": "null"}); body != crossSiteDenied {
		t.Fatalf("不可验证的 null Origin 未被拒绝：%s", body)
	}
}

func TestCrossSiteRequestIsRejected(t *testing.T) {
	body := postThroughCSRF(t, map[string]string{
		"Origin": "http://evil.example.com", "Sec-Fetch-Site": "cross-site",
	})
	if body != crossSiteDenied {
		t.Fatalf("跨站请求未被拒绝：%s", body)
	}
}

// Referrer-Policy 必须保留同源 Origin 与 Referer，否则上面两道防线同时失效。
func TestSecurityHeadersKeepSameOriginReferrer(t *testing.T) {
	w := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://203.0.113.10:8080/login", nil))
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q，必须为 same-origin", got)
	}
}
