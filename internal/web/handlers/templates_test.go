package handlers_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
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

// FR-005：模板详情要把「只能建不能拆」的不兼容原因讲清楚；用户级统计缺失只能作为提示（research.md C-005）。
func TestTemplateDetailExplainsCapabilityGapsInChinese(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	path := "/templates/" + templateID.String()

	// 有用户在监听但从未读到计数：给出 policy 提示，且不改变兼容状态。
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	_, body := app.Get(path)
	if !strings.Contains(body, "statsUserUplink") || !strings.Contains(body, "从未从节点读到任何用户级流量计数") {
		t.Fatalf("template detail does not hint at the missing policy: %s", body)
	}
	if !strings.Contains(body, "兼容") || strings.Contains(body, "不兼容") {
		t.Fatalf("the hint must not change compatibility: %s", body)
	}
	// 读到计数之后提示消失。
	app.SetTraffic(record, 4096, 4096)
	app.Collect()
	if _, body = app.Get(path); strings.Contains(body, "statsUserUplink") {
		t.Fatalf("the hint survived after counters were observed: %s", body)
	}

	// 只能建不能拆的节点：不兼容，且原因是中文。
	app.Adapter.Templates[templateID.String()] = ports.TemplateCapabilities{InboundCreatable: true, InboundRemovable: false,
		ProtocolSupported: true, MethodSupported: true, MultiUserSupported: true,
		CompatibilityReason: "node created the probe inbound but could not remove it"}
	if err := app.Validator.ValidateNow(context.Background(), templateID); err != nil {
		t.Fatal(err)
	}
	_, body = app.Get(path)
	if !strings.Contains(body, "不兼容") || !strings.Contains(body, "节点能创建入站但无法移除") {
		t.Fatalf("unremovable probe reason is not explained in Chinese: %s", body)
	}
}
