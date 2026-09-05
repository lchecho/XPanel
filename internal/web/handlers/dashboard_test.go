package handlers_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/testsupport"
)

func TestDashboardShowsCountsHealthAndZeroState(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	response, body := app.Get("/")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "尚未创建用户") || !strings.Contains(body, `hx-get="/fragments/dashboard-summary"`) ||
		!strings.Contains(body, `src="/static/htmx-2.0.10.min.js"`) || !strings.Contains(body, `src="/static/app.js"`) {
		t.Fatalf("dashboard zero state status=%d body=%s", response.StatusCode, body)
	}
	profileID := app.RegisterCompatibleProfile("Primary")
	limit := int64(1 << 20)
	active := app.CreateUser("Active", profileID, nil)
	exceeded := app.CreateUser("Exceeded", profileID, &limit)
	disabled := app.CreateUser("Disabled", profileID, nil)
	pending := app.CreateUser("Pending", profileID, nil)
	app.Drain()
	app.SetTraffic(active, 4096, 4096)
	app.SetTraffic(exceeded, 1<<20, 1<<20)
	app.Collect()
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	_ = pending
	app.Adapter.Available = false
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: pending.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain() // 失败 → retry_wait，实例被标记不可达
	app.Adapter.Available = true
	app.Collect() // 采集成功后实例恢复健康，陈旧标记消失

	_, body = app.Get("/")
	for _, want := range []string{"<dt>用户总数</dt><dd>4</dd>", "<dt>已启用</dt><dd>1</dd>", "<dt>手动禁用</dt><dd>2</dd>", "<dt>配额超限</dt><dd>1</dd>",
		"<dt>待同步</dt><dd>1</dd>", "2.01 MiB", "需要关注的同步操作", "节点暂时不可达", "健康"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "数据陈旧") {
		t.Fatalf("fresh dashboard marked stale: %s", body)
	}
}

func TestFragmentsAreLayoutFreeStaleAwareAndNeverCallXray(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	record := app.CreateUser("Alice", profileID, nil)
	app.Drain()
	app.SetTraffic(record, 100, 200)
	app.Collect()
	callsBefore := len(app.Adapter.Calls)
	response, body := app.Get("/fragments/dashboard-summary")
	if response.StatusCode != http.StatusOK || strings.Contains(body, "<html") || strings.Contains(body, "<nav") ||
		!strings.Contains(strings.Join(response.Header.Values("Vary"), ","), "HX-Request") || strings.Contains(body, "数据陈旧") ||
		!strings.Contains(body, "<dt>用户总数</dt><dd>1</dd>") {
		t.Fatalf("summary fragment status=%d vary=%q body=%s", response.StatusCode, response.Header.Values("Vary"), body)
	}
	response, body = app.Get("/fragments/users-table?q=ali&status=active")
	if response.StatusCode != http.StatusOK || strings.Contains(body, "<html") || !strings.Contains(body, "Alice") || !strings.Contains(body, "300 B") {
		t.Fatalf("users fragment status=%d body=%s", response.StatusCode, body)
	}
	if _, body = app.Get("/fragments/users-table?q=zzz"); !strings.Contains(body, "没有符合条件的用户") {
		t.Fatalf("filtered fragment body=%s", body)
	}
	if len(app.Adapter.Calls) != callsBefore {
		t.Fatalf("fragments called the Xray adapter: %d → %d", callsBefore, len(app.Adapter.Calls))
	}
	// 采集停止超过两个周期：陈旧标记出现，最后确认数据保留。
	app.Clock.Advance(11 * time.Second)
	_, body = app.Get("/fragments/dashboard-summary")
	if !strings.Contains(body, "数据陈旧") || !strings.Contains(body, "300 B") {
		t.Fatalf("stale summary body=%s", body)
	}
	_, body = app.Get("/fragments/users-table")
	if !strings.Contains(body, "数据陈旧") || !strings.Contains(body, "Alice") {
		t.Fatalf("stale users fragment body=%s", body)
	}
	// 采集恢复后陈旧标记消失。
	app.Collect()
	if _, body = app.Get("/fragments/dashboard-summary"); strings.Contains(body, "数据陈旧") {
		t.Fatalf("stale badge did not clear: %s", body)
	}
	anonymous := testsupport.New(t)
	if response, _ = anonymous.Get("/fragments/dashboard-summary"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous fragment status=%d", response.StatusCode)
	}
	_, body = app.Get("/users?q=ali&status=active")
	if !strings.Contains(body, `hx-get="/fragments/users-table?q=ali&amp;status=active"`) {
		t.Fatalf("users page does not preserve filters in the fragment URL: %s", body)
	}
}
