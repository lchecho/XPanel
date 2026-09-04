package handlers_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

func profileForm(t *testing.T, name, tag string) (url.Values, string) {
	t.Helper()
	key, err := security.GenerateUserKey(security.MethodAES256)
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{"name": {name}, "inbound_tag": {tag}, "public_host": {"vpn.example.com"}, "public_port": {"8388"},
		"method": {security.MethodAES256}, "network": {"tcp_udp"}, "server_key": {key.Reveal()}, "bootstrap_statistics_id": {"bootstrap"}}, key.Reveal()
}

func TestProfilePagesRequireAuthenticationAndNeverEchoKeys(t *testing.T) {
	app := testsupport.New(t)
	response, body := app.Get("/profiles")
	if response.StatusCode != http.StatusSeeOther || strings.Contains(body, "server_key") {
		t.Fatalf("unauthenticated profiles status=%d body=%q", response.StatusCode, body)
	}
	app.Login()
	response, body = app.Get("/profiles/new")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `label for="server_key"`) || !strings.Contains(body, `aria-describedby="name-error"`) {
		t.Fatalf("new profile form status=%d body=%s", response.StatusCode, body)
	}
	form, key := profileForm(t, "Primary", "managed")
	response, body = app.PostForm("/profiles", "/profiles/new", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create profile status=%d body=%s", response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	response, body = app.Get(location)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "待验证") || strings.Contains(body, key) {
		t.Fatalf("detail status=%d contains key=%v body=%s", response.StatusCode, strings.Contains(body, key), body)
	}
	if !strings.Contains(body, "访问配置已保存") {
		t.Fatalf("flash message missing: %s", body)
	}
	id := domain.ID(strings.TrimPrefix(location, "/profiles/"))
	if err := app.Validator.ValidateNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	_, body = app.Get(location)
	if !strings.Contains(body, "兼容") || strings.Contains(body, "待验证") {
		t.Fatalf("validated detail body=%s", body)
	}
	_, body = app.Get(location + "/edit")
	if strings.Contains(body, key) || !strings.Contains(body, `name="server_key" type="password" autocomplete="off" value=""`) {
		t.Fatalf("edit form echoed the key or lacks blank key input: %s", body)
	}
	if !strings.Contains(body, "不会校验该密钥是否与入站一致") {
		t.Fatal("edit form lacks the service key disclaimer")
	}
}

func TestProfileFormValidationConflictAndRevalidate(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	form, _ := profileForm(t, "Primary", "managed")
	form.Set("public_port", "70000")
	response, body := app.PostForm("/profiles", "/profiles/new", form)
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "端口必须在 1 到 65535 之间") ||
		!strings.Contains(body, `value="vpn.example.com"`) || strings.Contains(body, form.Get("server_key")) {
		t.Fatalf("validation status=%d body=%s", response.StatusCode, body)
	}
	form.Set("public_port", "8388")
	response, _ = app.PostForm("/profiles", "/profiles/new", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d", response.StatusCode)
	}
	location := response.Header.Get("Location")

	edit := url.Values{"name": {"Primary"}, "inbound_tag": {"managed"}, "public_host": {"edge.example.com"}, "public_port": {"8388"},
		"method": {security.MethodAES256}, "network": {"tcp_udp"}, "server_key": {""}, "bootstrap_statistics_id": {"bootstrap"}, "_version": {"7"}}
	response, body = app.PostForm(location, location+"/edit", edit)
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "访问配置已被修改") || !strings.Contains(body, `name="_version" value="0"`) {
		t.Fatalf("stale version status=%d body=%s", response.StatusCode, body)
	}
	edit.Set("_version", "0")
	response, _ = app.PostForm(location, location+"/edit", edit)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit status=%d", response.StatusCode)
	}
	_, body = app.Get(location)
	if !strings.Contains(body, "edge.example.com") {
		t.Fatalf("edit did not persist: %s", body)
	}
	response, _ = app.PostForm(location+"/revalidate", location, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("revalidate status=%d", response.StatusCode)
	}
	_, body = app.Get(location)
	if !strings.Contains(body, "已重新排队验证") {
		t.Fatalf("revalidate flash missing: %s", body)
	}
	response, _ = app.Get("/profiles/not-a-valid-id")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid id status=%d", response.StatusCode)
	}
}
