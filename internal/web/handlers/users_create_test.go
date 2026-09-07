package handlers_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"xpanel/internal/testsupport"
)

func TestUserCreationFormAndConnectionVisibility(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	response, body := app.Get("/users/new")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "当前没有处于“兼容”状态的入站模板") {
		t.Fatalf("zero-state form status=%d body=%s", response.StatusCode, body)
	}
	_, body = app.Get("/users")
	if !strings.Contains(body, "尚未创建用户") {
		t.Fatalf("users list zero state missing: %s", body)
	}
	templateID := app.RegisterCompatibleTemplate("Primary")
	_, body = app.Get("/users/new")
	if !strings.Contains(body, `<option value="`+templateID.String()+`"`) || !strings.Contains(body, `name="reset_day" type="number" min="1" max="28" value="1"`) {
		t.Fatalf("form lacks compatible template option or reset day default: %s", body)
	}

	// 端口字段可选：表单展示所选模板的端口池区间，留空表示自动分配（FR-007）。
	if !strings.Contains(body, `端口池 30000–30099`) || !strings.Contains(body, `id="port" name="port"`) ||
		strings.Contains(body, `name="server_key"`) {
		t.Fatalf("port field or pool range missing from the form: %s", body)
	}

	invalid := url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()}, "quota_value": {"0"}, "quota_unit": {"GiB"}, "reset_day": {"1"}}
	response, body = app.PostForm("/users", "/users/new", invalid)
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "配额必须为正整数") || !strings.Contains(body, `value="Alice"`) {
		t.Fatalf("invalid quota status=%d body=%s", response.StatusCode, body)
	}
	missingQuota := url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()}, "quota_value": {""}, "quota_unit": {"GiB"}, "reset_day": {"1"}}
	if response, _ = app.PostForm("/users", "/users/new", missingQuota); response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty quota without unlimited status=%d", response.StatusCode)
	}

	// 端口非数字 / 池外端口是字段级 422，页面保留已填内容并给出允许范围；池内端口被接受。
	nonNumeric := url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()}, "port": {"abc"}, "unlimited": {"on"}, "reset_day": {"1"}}
	if response, body = app.PostForm("/users", "/users/new", nonNumeric); response.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "端口必须位于端口池内") {
		t.Fatalf("non-numeric port status=%d body=%s", response.StatusCode, body)
	}
	outside := url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()}, "port": {"40000"}, "unlimited": {"on"}, "reset_day": {"1"}}
	if response, body = app.PostForm("/users", "/users/new", outside); response.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "端口必须位于端口池内") || !strings.Contains(body, `端口池 30000–30099`) {
		t.Fatalf("out-of-pool port status=%d body=%s", response.StatusCode, body)
	}

	valid := url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()}, "quota_value": {"2"}, "quota_unit": {"GiB"}, "reset_day": {"5"}}
	response, body = app.PostForm("/users", "/users/new", valid)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d body=%s", response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	response, body = app.Get(location)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "启用中（待同步）") || !strings.Contains(body, "2.00 GiB") ||
		!strings.Contains(body, "连接信息将在节点确认凭证后可用") {
		t.Fatalf("detail before sync status=%d body=%s", response.StatusCode, body)
	}
	// 详情页展示端口、入站标签与监听状态；同步前是「待同步」。
	if !strings.Contains(body, "<dt>专属端口</dt><dd>30000</dd>") || !strings.Contains(body, "<dt>入站标签</dt><dd>xpanel-") ||
		!strings.Contains(body, "<dt>监听状态</dt><dd>待同步</dd>") {
		t.Fatalf("detail lacks port, inbound tag or listening state: %s", body)
	}
	response, body = app.Get(location + "/connection")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "连接信息尚不可用") || strings.Contains(body, "ss://") ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("pending connection status=%d cache=%q body=%s", response.StatusCode, response.Header.Get("Cache-Control"), body)
	}

	app.Drain()
	_, body = app.Get(location)
	if !strings.Contains(body, "已启用") || strings.Contains(body, "待同步") || !strings.Contains(body, "查看连接信息") {
		t.Fatalf("detail after sync body=%s", body)
	}
	if !strings.Contains(body, "<dt>监听状态</dt><dd>监听中</dd>") {
		t.Fatalf("detail after sync does not report the port as listening: %s", body)
	}
	// 指定一个已被占用的端口是 409，并给出更换端口的提示（contracts/http.md）。
	taken := url.Values{"display_name": {"Bob"}, "template_id": {templateID.String()}, "port": {"30000"}, "unlimited": {"on"}, "reset_day": {"1"}}
	if response, body = app.PostForm("/users", "/users/new", taken); response.StatusCode != http.StatusConflict ||
		!strings.Contains(body, "该端口已分配给其他用户") {
		t.Fatalf("occupied port status=%d body=%s", response.StatusCode, body)
	}
	_, body = app.Get(location + "/connection")
	if !strings.Contains(body, "ss://") || !strings.Contains(body, "vpn.example.com") || !strings.Contains(body, `class="print-hidden connection-secret"`) {
		t.Fatalf("connection page body=%s", body)
	}

	duplicate := url.Values{"display_name": {" alice "}, "template_id": {templateID.String()}, "unlimited": {"on"}, "reset_day": {"1"}}
	response, body = app.PostForm("/users", "/users/new", duplicate)
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "显示名称已被使用") {
		t.Fatalf("duplicate name status=%d body=%s", response.StatusCode, body)
	}

	anonymous := testsupport.New(t)
	response, body = anonymous.Get("/users/" + strings.TrimPrefix(location, "/users/") + "/connection")
	if response.StatusCode != http.StatusSeeOther || strings.Contains(body, "ss://") {
		t.Fatalf("anonymous connection status=%d body=%s", response.StatusCode, body)
	}
}
