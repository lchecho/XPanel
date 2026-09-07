package e2e

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"xpanel/internal/domain"
)

var passwordPattern = regexp.MustCompile(`<code class="print-hidden connection-secret">([^<]+)</code>`)

func TestLifecycleEndToEnd(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	response, _ := app.PostForm("/users", "/users/new", url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()},
		"unlimited": {"on"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d", response.StatusCode)
	}
	path := response.Header.Get("Location")
	userID := domain.ID(strings.TrimPrefix(path, "/users/"))
	app.Drain()
	record := app.User(userID)
	statsID := record.Identity.StatisticsID
	// 专属入站标签在整个生命周期内不变；停用会移除整条入站，启用会按同一端口重建（FR-017/FR-019）。
	tag := record.Inbound.Inbound.InboundTag
	port := record.Inbound.Inbound.Port
	present := func() bool { _, ok := app.Adapter.Users[tag][statsID]; return ok }
	app.SetTraffic(record, 4096, 8192)
	app.Collect()

	// 编辑显示名称。
	response, _ = app.PostForm(path, path+"/edit", url.Values{"display_name": {"Alice Prime"}, "unlimited": {"on"}, "reset_day": {"1"}, "admin_enabled": {"on"}, "_version": {"0"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit status=%d", response.StatusCode)
	}
	// 禁用（重复提交同一请求）→ 启用。
	requestID := url.Values{"_request_id": {app.User(userID).User.ID.String()[:8] + "-0000-4000-8000-000000000001"}, "_version": {"1"}}
	for i := 0; i < 2; i++ {
		if response, _ = app.PostForm(path+"/disable", path, requestID); response.StatusCode != http.StatusSeeOther {
			t.Fatalf("disable #%d status=%d", i, response.StatusCode)
		}
	}
	app.Drain()
	if present() {
		t.Fatal("disabled user still present in fake Xray")
	}
	if response, _ = app.PostForm(path+"/disable", path, url.Values{"_version": {"1"}}); response.StatusCode != http.StatusConflict {
		t.Fatalf("stale version status=%d", response.StatusCode)
	}
	if response, _ = app.PostForm(path+"/enable", path, url.Values{"_version": {"2"}}); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable status=%d", response.StatusCode)
	}
	app.Drain()
	if !present() || app.User(userID).Cycle.GrossDownlinkBytes != 8192 {
		t.Fatal("re-enabled user missing or history lost")
	}
	if got := app.User(userID).Inbound.Inbound.Port; got != port || !app.Listening(port) {
		t.Fatalf("re-enabled port = %d want %d listening", got, port)
	}

	// 轮换：旧凭证消失、新凭证出现、历史归属不变。
	_, body := app.Get(path + "/connection")
	oldPassword := passwordPattern.FindStringSubmatch(body)
	if len(oldPassword) != 2 {
		t.Fatalf("no password on connection page: %s", body)
	}
	if response, _ = app.PostForm(path+"/rotate", path+"/rotate", url.Values{"_version": {"3"}}); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("rotate status=%d", response.StatusCode)
	}
	app.Drain()
	remote := app.Adapter.Users[tag][statsID]
	if remote.CredentialVersion != 2 {
		t.Fatalf("fake Xray credential version = %d", remote.CredentialVersion)
	}
	_, body = app.Get(path + "/connection")
	newPassword := passwordPattern.FindStringSubmatch(body)
	if len(newPassword) != 2 || newPassword[1] == oldPassword[1] {
		t.Fatal("rotation did not change the client password")
	}
	after := app.User(userID)
	if after.Cycle.GrossDownlinkBytes != 8192 || after.Identity.StatisticsID != statsID {
		t.Fatal("rotation changed traffic history or identity")
	}

	// 轮换期间端口不中断：轮换前后都在监听，且始终是同一个端口。
	if !app.Listening(port) || app.User(userID).Inbound.Inbound.Port != port {
		t.Fatalf("rotation changed or stopped port %d", port)
	}

	// 删除：软删除、移除、名称复用得到新身份。
	if response, _ = app.PostForm(path+"/delete", path+"/delete", url.Values{"_version": {"4"}}); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status=%d", response.StatusCode)
	}
	app.Drain()
	if present() {
		t.Fatal("deleted user still present in fake Xray")
	}
	if app.Listening(port) {
		t.Fatalf("port %d kept listening after deletion", port)
	}
	_, body = app.Get("/users")
	if strings.Contains(body, ">Alice Prime<") {
		t.Fatalf("deleted user listed by default: %s", body)
	}
	_, body = app.Get("/users?deleted=1")
	if !strings.Contains(body, "Alice Prime") || !strings.Contains(body, "已删除") {
		t.Fatalf("deleted user missing from inclusive list: %s", body)
	}
	var destroyed int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM access_credentials WHERE allocation_id=? AND state!='destroyed'`, record.Allocation.ID.String()).Scan(&destroyed)
	if destroyed != 0 {
		t.Fatalf("credentials still hold key material after delete: %d", destroyed)
	}
	response, _ = app.PostForm("/users", "/users/new", url.Values{"display_name": {"Alice Prime"}, "template_id": {templateID.String()},
		"unlimited": {"on"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("recreate status=%d", response.StatusCode)
	}
	reused := app.User(domain.ID(strings.TrimPrefix(response.Header.Get("Location"), "/users/")))
	if reused.User.ID == userID || reused.Identity.StatisticsID == statsID || reused.Cycle.GrossDownlinkBytes != 0 {
		t.Fatal("recreated user reused the old identity or history")
	}
	// 释放的端口回到池中，被新用户以全新入站标签重新占用。
	if reused.Inbound.Inbound.Port != port || reused.Inbound.Inbound.InboundTag == tag {
		t.Fatalf("recreated user port=%d tag=%s (previous port=%d tag=%s)", reused.Inbound.Inbound.Port,
			reused.Inbound.Inbound.InboundTag, port, tag)
	}
	app.Drain()
	if !app.Listening(port) {
		t.Fatalf("port %d did not come back for the new user", port)
	}
	var audits int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE target_id=? AND action IN (?,?,?,?,?)`, userID.String(),
		domain.ActionUserUpdated, domain.ActionUserDisabled, domain.ActionUserEnabled, domain.ActionCredentialRotated, domain.ActionUserDeleted).Scan(&audits)
	if audits < 6 {
		t.Fatalf("lifecycle audits = %d", audits)
	}
}
