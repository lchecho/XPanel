package handlers_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

func templateForm(t *testing.T, name string, poolStart, poolEnd int) url.Values {
	t.Helper()
	return url.Values{"name": {name}, "public_host": {"vpn.example.com"}, "listen_address": {"127.0.0.1"},
		"port_pool_start": {strconv.Itoa(poolStart)}, "port_pool_end": {strconv.Itoa(poolEnd)},
		"method": {security.MethodAES256}, "network": {"tcp_udp"}}
}

// 入站模板页面需要认证；表单不再提供服务端密钥字段（FR-004）。
func TestTemplatePagesRequireAuthenticationAndExposeNoKeys(t *testing.T) {
	app := testsupport.New(t)
	response, body := app.Get("/templates")
	if response.StatusCode != http.StatusSeeOther || strings.Contains(body, "server_key") {
		t.Fatalf("unauthenticated templates status=%d body=%q", response.StatusCode, body)
	}
	app.Login()
	response, body = app.Get("/templates/new")
	if response.StatusCode != http.StatusOK || strings.Contains(body, `name="server_key"`) ||
		!strings.Contains(body, `label for="listen_address"`) || !strings.Contains(body, `label for="port_pool_start"`) ||
		!strings.Contains(body, `aria-describedby="name-error"`) {
		t.Fatalf("new template form status=%d body=%s", response.StatusCode, body)
	}
	response, body = app.PostForm("/templates", "/templates/new", templateForm(t, "Primary", 30000, 30099))
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create template status=%d body=%s", response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	response, body = app.Get(location)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "待验证") || !strings.Contains(body, "30000–30099") {
		t.Fatalf("detail status=%d body=%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "入站模板已保存") {
		t.Fatalf("flash message missing: %s", body)
	}
	id := domain.ID(strings.TrimPrefix(location, "/templates/"))
	if err := app.Validator.ValidateNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	_, body = app.Get(location)
	if !strings.Contains(body, "兼容") || strings.Contains(body, "待验证") {
		t.Fatalf("validated detail body=%s", body)
	}
	_, body = app.Get(location + "/edit")
	if strings.Contains(body, `name="server_key"`) || !strings.Contains(body, "服务端密钥由面板生成") {
		t.Fatalf("edit form still asks for a server key: %s", body)
	}
}

// 字段级校验、乐观并发冲突与重新验证。
func TestTemplateFormValidationConflictAndRevalidate(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	form := templateForm(t, "Primary", 30100, 30000) // 区间倒置
	response, body := app.PostForm("/templates", "/templates/new", form)
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "不小于起始端口") ||
		!strings.Contains(body, `value="vpn.example.com"`) {
		t.Fatalf("validation status=%d body=%s", response.StatusCode, body)
	}
	form = templateForm(t, "Primary", 30000, 30099)
	response, _ = app.PostForm("/templates", "/templates/new", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d", response.StatusCode)
	}
	location := response.Header.Get("Location")

	edit := templateForm(t, "Primary", 30000, 30099)
	edit.Set("public_host", "edge.example.com")
	edit.Set("_version", "7")
	response, body = app.PostForm(location, location+"/edit", edit)
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "入站模板已被修改") || !strings.Contains(body, `name="_version" value="0"`) {
		t.Fatalf("stale version status=%d body=%s", response.StatusCode, body)
	}
	edit.Set("_version", "0")
	response, _ = app.PostForm(location, location+"/edit", edit)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit status=%d", response.StatusCode)
	}

	// 重新验证需要当前版本；过期版本冲突。
	response, _ = app.PostForm(location+"/revalidate", location, url.Values{"_version": {"0"}})
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale revalidate status=%d", response.StatusCode)
	}
	response, _ = app.PostForm(location+"/revalidate", location, url.Values{"_version": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("revalidate status=%d", response.StatusCode)
	}
}
