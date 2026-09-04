package handlers_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"xpanel/internal/testsupport"
)

func TestUserEditFormValidationConflictAndReset(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	limit := int64(2 << 30)
	record := app.CreateUser("Alice", profileID, &limit)
	app.Drain()
	path := "/users/" + record.User.ID.String()

	response, body := app.Get(path + "/edit")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `name="quota_value" type="number" min="1" inputmode="numeric" value="2"`) ||
		!strings.Contains(body, `<option value="GiB" selected>`) || !strings.Contains(body, `name="admin_enabled" value="on" checked`) {
		t.Fatalf("edit form status=%d body=%s", response.StatusCode, body)
	}
	invalid := url.Values{"display_name": {"Alice"}, "quota_value": {"2"}, "quota_unit": {"GiB"}, "reset_day": {"40"}, "admin_enabled": {"on"}, "_version": {"0"}}
	response, body = app.PostForm(path, path+"/edit", invalid)
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "重置日必须在 1 到 28 之间") || !strings.Contains(body, `id="reset_day-error"`) {
		t.Fatalf("invalid reset day status=%d body=%s", response.StatusCode, body)
	}
	stale := url.Values{"display_name": {"Alice Renamed"}, "quota_value": {"2"}, "quota_unit": {"GiB"}, "reset_day": {"1"}, "admin_enabled": {"on"}, "_version": {"9"}}
	response, body = app.PostForm(path, path+"/edit", stale)
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "用户已被修改") && !strings.Contains(body, "user changed") ||
		!strings.Contains(body, `name="_version" value="0"`) || !strings.Contains(body, `value="Alice Renamed"`) {
		t.Fatalf("stale version status=%d body=%s", response.StatusCode, body)
	}
	valid := url.Values{"display_name": {"Alice Renamed"}, "quota_value": {"3"}, "quota_unit": {"GiB"}, "reset_day": {"7"}, "admin_enabled": {"on"}, "_version": {"0"}}
	response, _ = app.PostForm(path, path+"/edit", valid)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit status=%d", response.StatusCode)
	}
	_, body = app.Get(path)
	if !strings.Contains(body, "Alice Renamed") || !strings.Contains(body, "3.00 GiB") || !strings.Contains(body, "每月 7 日重置") || !strings.Contains(body, "用户已更新") {
		t.Fatalf("detail after edit body=%s", body)
	}

	response, body = app.Get(path + "/reset-traffic")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "周期结束时间") || !strings.Contains(body, "每日趋势保持不变") {
		t.Fatalf("reset page status=%d body=%s", response.StatusCode, body)
	}
	response, _ = app.PostForm(path+"/reset-traffic", path+"/reset-traffic", nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset status=%d", response.StatusCode)
	}
	_, body = app.Get(path)
	if !strings.Contains(body, "本周期用量已清零") {
		t.Fatalf("reset flash missing: %s", body)
	}
}
