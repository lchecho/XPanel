package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

const mib = 1 << 20

func TestQuotaLifecycleEndToEnd(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	response, _ := app.PostForm("/users", "/users/new", url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()},
		"quota_value": {"2"}, "quota_unit": {"MiB"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d", response.StatusCode)
	}
	path := response.Header.Get("Location")
	userID := domain.ID(strings.TrimPrefix(path, "/users/"))
	app.Drain()
	record := app.User(userID)
	statsID := record.Identity.StatisticsID
	tag := record.Inbound.Inbound.InboundTag
	port := record.Inbound.Inbound.Port
	present := func() bool { _, ok := app.Adapter.Users[tag][statsID]; return ok }
	// 封禁与恢复都必须作用在同一个端口上：SC-005 要求以原端口恢复。
	assertPort := func(step string, listening bool) {
		t.Helper()
		if app.Listening(port) != listening {
			t.Fatalf("%s: port %d listening = %v want %v", step, port, app.Listening(port), listening)
		}
		if current := app.User(userID).Inbound.Inbound.Port; current != port {
			t.Fatalf("%s: port changed to %d", step, current)
		}
	}
	assertPort("baseline", true)

	// 越界：小额配额 → 计数增长 → exceeded → 同步移除。
	app.SetTraffic(record, mib, mib)
	if summary := app.Collect(); summary.Blocked != 1 {
		t.Fatalf("crossing summary = %#v", summary)
	}
	app.Drain()
	if present() {
		t.Fatal("quota-exceeded user still present in fake Xray")
	}
	assertPort("quota_block", false)
	_, body := app.Get(path)
	if !strings.Contains(body, "配额超限") || !strings.Contains(body, "已建立的连接可能继续并造成少量超额") || !strings.Contains(body, "2.00 MiB") {
		t.Fatalf("detail at quota body=%s", body)
	}
	if !strings.Contains(body, "已停止监听") || !strings.Contains(body, "<dt>监听状态</dt><dd>未监听</dd>") {
		t.Fatalf("detail does not explain that the port stopped listening: %s", body)
	}
	_, body = app.Get(path + "/connection")
	if !strings.Contains(body, "该用户当前不活跃") || !strings.Contains(body, "ss://") {
		t.Fatalf("connection page for exceeded user body=%s", body)
	}

	// 新周期自动恢复。
	cycleEnd := app.User(userID).Cycle.EndsAt
	app.Clock.Set(cycleEnd.Add(time.Second))
	if app.Rollover() != 1 {
		t.Fatal("rollover did not run at the boundary")
	}
	app.Drain()
	if !present() {
		t.Fatal("quota-blocked user was not restored after rollover")
	}
	assertPort("quota_restore", true)
	_, body = app.Get(path)
	if !strings.Contains(body, "已启用") || !strings.Contains(body, "<dd>0 B（上行 0 B / 下行 0 B）</dd>") {
		t.Fatalf("detail after rollover body=%s", body)
	}

	// 手动禁用者在周期切换后不恢复。
	response, _ = app.PostForm(path, path+"/edit", url.Values{"display_name": {"Alice"}, "quota_value": {"2"}, "quota_unit": {"MiB"}, "reset_day": {"1"}, "_version": {"0"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable status=%d", response.StatusCode)
	}
	app.Drain()
	if present() {
		t.Fatal("manually disabled user still present")
	}
	app.Clock.Set(app.User(userID).Cycle.EndsAt.Add(time.Second))
	app.Rollover()
	app.Drain()
	if present() || app.User(userID).Allocation.DisplayState(app.User(userID).User) != domain.DisplayDisabled {
		t.Fatal("manually disabled user was restored by rollover")
	}
	assertPort("manual_disable", false)

	// 重新启用后：调低即时封禁，调高即时恢复。
	response, _ = app.PostForm(path, path+"/edit", url.Values{"display_name": {"Alice"}, "quota_value": {"2"}, "quota_unit": {"MiB"}, "reset_day": {"1"}, "admin_enabled": {"on"}, "_version": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable status=%d", response.StatusCode)
	}
	app.Drain()
	if !present() {
		t.Fatal("re-enabled user not present")
	}
	assertPort("re_enable", true)
	app.SetTraffic(record, 2*mib+mib/2, mib) // 新周期内增量 1.5 MiB
	app.Collect()
	if app.User(userID).Allocation.QuotaState != domain.QuotaWithinLimit {
		t.Fatal("usage below quota reported as exceeded")
	}
	response, _ = app.PostForm(path, path+"/edit", url.Values{"display_name": {"Alice"}, "quota_value": {"1"}, "quota_unit": {"MiB"}, "reset_day": {"1"}, "admin_enabled": {"on"}, "_version": {"2"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("lower status=%d", response.StatusCode)
	}
	app.Drain()
	if present() || app.User(userID).Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatal("lowering quota below usage did not block immediately")
	}
	response, _ = app.PostForm(path, path+"/edit", url.Values{"display_name": {"Alice"}, "unlimited": {"on"}, "reset_day": {"1"}, "admin_enabled": {"on"}, "_version": {"3"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("raise status=%d", response.StatusCode)
	}
	app.Drain()
	if !present() {
		t.Fatal("raising quota to unlimited did not restore immediately")
	}

	// 手动重置：清零 accounted、周期结束时间不变、恢复访问。
	response, _ = app.PostForm(path, path+"/edit", url.Values{"display_name": {"Alice"}, "quota_value": {"1"}, "quota_unit": {"MiB"}, "reset_day": {"1"}, "admin_enabled": {"on"}, "_version": {"4"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-lower status=%d", response.StatusCode)
	}
	app.Drain()
	before := app.User(userID)
	if present() || before.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatal("expected exceeded before reset")
	}
	response, _ = app.PostForm(path+"/reset-traffic", path+"/reset-traffic", nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset status=%d", response.StatusCode)
	}
	app.Drain()
	after := app.User(userID)
	if !present() || after.Cycle.AccountedUplinkBytes != 0 || !after.Cycle.EndsAt.Equal(before.Cycle.EndsAt) || after.Cycle.GrossUplinkBytes != before.Cycle.GrossUplinkBytes {
		t.Fatalf("after reset cycle=%#v present=%v", after.Cycle, present())
	}
	var resets int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM quota_reset_events WHERE actor_id=?`, app.AdminID.String()).Scan(&resets)
	if resets != 1 {
		t.Fatalf("reset events by admin = %d", resets)
	}
	_ = ports.Uplink
}
