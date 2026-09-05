package handlers_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"xpanel/internal/testsupport"
)

// T141：模板引用内容哈希资源名；仅哈希资源返回 immutable 缓存头，HTML 与未哈希路径不可长期缓存。
func TestStaticAssetsAreContentHashedAndImmutable(t *testing.T) {
	app := testsupport.New(t)
	response, body := app.Get("/login")
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("login page status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	pattern := regexp.MustCompile(`href="(/static/app\.[0-9a-f]{12}\.css)"`)
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("hashed stylesheet reference missing: %s", body)
	}
	for _, ref := range []string{match[1]} {
		response, content := app.Get(ref)
		if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
			!strings.HasPrefix(response.Header.Get("Content-Type"), "text/css") || len(content) == 0 {
			t.Fatalf("asset %s status=%d cache=%q type=%q", ref, response.StatusCode, response.Header.Get("Cache-Control"), response.Header.Get("Content-Type"))
		}
		if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("security headers missing on asset: %v", response.Header)
		}
	}
	for _, stale := range []string{"/static/app.css", "/static/app.000000000000.css", "/static/THIRD_PARTY.md"} {
		response, _ := app.Get(stale)
		if response.StatusCode != http.StatusNotFound || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s status=%d cache=%q", stale, response.StatusCode, response.Header.Get("Cache-Control"))
		}
	}
	app.Login()
	response, body = app.Get("/")
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("dashboard status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	scripts := regexp.MustCompile(`src="(/static/(?:htmx-2\.0\.10\.min|app)\.[0-9a-f]{12}\.js)"`).FindAllStringSubmatch(body, -1)
	if len(scripts) != 2 {
		t.Fatalf("expected two hashed scripts, body=%s", body)
	}
	for _, script := range scripts {
		response, _ := app.Get(script[1])
		if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "javascript") {
			t.Fatalf("script %s status=%d type=%q", script[1], response.StatusCode, response.Header.Get("Content-Type"))
		}
	}
}
