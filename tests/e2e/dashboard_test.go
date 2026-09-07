package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/testsupport"
)

func TestDashboardEndToEnd(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	alice := app.CreateUser("Alice", templateID, nil)
	bob := app.CreateUser("Bob", templateID, &limit)
	carol := app.CreateUser("Carol", templateID, nil)
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
	// 端口维度的可观测性：池容量、已分配与剩余可分配，三者随分配变化（FR-035）。
	capacity := testsupport.DefaultPoolEnd - testsupport.DefaultPoolStart + 1
	for _, want := range []string{"<dt>池容量</dt><dd>" + strconv.Itoa(capacity) + "</dd>", "<dt>已分配</dt><dd>3</dd>",
		"<dt>剩余可分配</dt><dd>" + strconv.Itoa(capacity-3) + "</dd>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard port pool missing %q: %s", want, body)
		}
	}
	// 无脚本路径：整页已包含完整汇总与表格数据，含每个用户的端口与监听状态。
	_, body = app.Get("/users")
	alicePort := strconv.Itoa(app.User(alice.User.ID).Inbound.Inbound.Port)
	for _, want := range []string{"Alice", "Bob", "Carol", "配额超限", "手动禁用", "4 KiB",
		`<td data-label="端口">` + alicePort + `</td>`, `<td data-label="监听">监听中</td>`, `<td data-label="监听">未监听</td>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("users page missing %q: %s", want, body)
		}
	}
	// 从端口反查用户。
	_, body = app.Get("/users?q=" + alicePort)
	if !strings.Contains(body, "Alice") || strings.Contains(body, ">Bob<") {
		t.Fatalf("port lookup returned the wrong rows: %s", body)
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
