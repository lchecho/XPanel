package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/testsupport"
)

func TestDashboardEndToEnd(t *testing.T) {
	app := newHarness(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	limit := int64(1 << 20)
	alice := app.CreateUser("Alice", profileID, nil)
	bob := app.CreateUser("Bob", profileID, &limit)
	carol := app.CreateUser("Carol", profileID, nil)
	app.Drain()
	app.SetTraffic(alice, 2048, 2048)
	app.SetTraffic(bob, 1<<20, 0)
	app.Collect()
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: carol.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()

	_, body := app.Get("/")
	for _, want := range []string{"<dt>用户总数</dt><dd>3</dd>", "<dt>已启用</dt><dd>1</dd>", "<dt>手动禁用</dt><dd>1</dd>", "<dt>配额超限</dt><dd>1</dd>", "1.00 MiB", "健康"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}
	// 无脚本路径：整页已包含完整汇总与表格数据。
	_, body = app.Get("/users")
	for _, want := range []string{"Alice", "Bob", "Carol", "配额超限", "手动禁用", "4 KiB"} {
		if !strings.Contains(body, want) {
			t.Fatalf("users page missing %q: %s", want, body)
		}
	}
	// 采集失败：最后确认值保留并标记陈旧，最近故障可见。
	app.Adapter.Available = false
	app.Clock.Advance(11 * time.Second)
	if _, err := app.Traffic.CollectOnce(context.Background()); err == nil {
		t.Fatal("collection succeeded while Xray is unavailable")
	}
	_, body = app.Get("/")
	if !strings.Contains(body, "数据陈旧") || !strings.Contains(body, "不可达") || !strings.Contains(body, "1.00 MiB") || !strings.Contains(body, "节点暂时不可达") {
		t.Fatalf("stale dashboard body=%s", body)
	}
	_, body = app.Get("/fragments/users-table?status=quota_exceeded")
	if !strings.Contains(body, "Bob") || strings.Contains(body, ">Alice<") || !strings.Contains(body, "数据陈旧") {
		t.Fatalf("filtered stale fragment body=%s", body)
	}
	// 详情页每日趋势与节点状态。
	_, body = app.Get("/users/" + alice.User.ID.String())
	if !strings.Contains(body, "当前周期每日趋势") || !strings.Contains(body, "2 KiB") || !strings.Contains(body, "不可达") {
		t.Fatalf("detail trend body=%s", body)
	}
	_ = time.Second
}
