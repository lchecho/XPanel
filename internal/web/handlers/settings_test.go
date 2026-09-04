package handlers_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"xpanel/internal/testsupport"
)

func TestSettingsTimezoneValidationAndVersion(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	response, body := app.Get("/settings")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<strong>UTC</strong>") || !strings.Contains(body, `name="_version" value="0"`) {
		t.Fatalf("settings page status=%d body=%s", response.StatusCode, body)
	}
	response, body = app.PostForm("/settings", "/settings", url.Values{"quota_timezone": {"Mars/Olympus"}, "_version": {"0"}})
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "IANA") || !strings.Contains(body, `value="Mars/Olympus"`) {
		t.Fatalf("invalid timezone status=%d body=%s", response.StatusCode, body)
	}
	response, _ = app.PostForm("/settings", "/settings", url.Values{"quota_timezone": {"Asia/Shanghai"}, "_version": {"0"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("update status=%d", response.StatusCode)
	}
	_, body = app.Get("/settings")
	if !strings.Contains(body, "<strong>Asia/Shanghai</strong>") || !strings.Contains(body, "将从下一个配额周期起生效") || !strings.Contains(body, `name="_version" value="1"`) {
		t.Fatalf("settings after update body=%s", body)
	}
	response, body = app.PostForm("/settings", "/settings", url.Values{"quota_timezone": {"Europe/Berlin"}, "_version": {"0"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, `name="_version" value="1"`) {
		t.Fatalf("stale settings status=%d body=%s", response.StatusCode, body)
	}
	_, body = app.Get("/users")
	if !strings.Contains(body, "时间按面板配额时区显示：Asia/Shanghai") {
		t.Fatalf("footer timezone not updated: %s", body)
	}
}
