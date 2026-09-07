package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

func TestRecoveryAfterXrayRestartAndUncertainMutations(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	active := app.CreateUser("Active", templateID, nil)
	disabled := app.CreateUser("Disabled", templateID, nil)
	exceeded := app.CreateUser("Exceeded", templateID, &limit)
	deleted := app.CreateUser("Deleted", templateID, nil)
	pending := app.CreateUser("Pending", templateID, nil)
	app.Drain()
	app.SetTraffic(active, 4096, 4096)
	app.SetTraffic(exceeded, 1<<20, 1<<20)
	app.Collect()
	actor := app.AdminID
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: deleted.User.ID, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	// 一个待同步用户：轮换后 Xray 离线。
	app.Adapter.Available = false
	if _, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: pending.User.ID, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	if _, err := app.Reconcile.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("reconcile succeeded while Xray is unavailable")
	}
	// Xray 重启：面板入站全部丢失、boot epoch 变化；重启后运维自建入站与一条命名空间内的孤儿入站被重新注入。
	app.Clock.Advance(time.Minute)
	app.Adapter.Restart()
	app.Adapter.AddExternalInbound("operator-inbound", 45000)
	app.Adapter.AddOrphanInbound(domain.NamespacePrefix+"orphan", 30090)
	app.Adapter.Available = true
	// 重启窗口内：所有面板端口都不在监听，界面明确告知端口暂时不可用（FR-028/SC-006）。
	for _, record := range []ports.UserRecord{active, pending} {
		if app.Listening(app.User(record.User.ID).Inbound.Inbound.Port) {
			t.Fatalf("port of %s survived the restart", record.User.DisplayName)
		}
	}
	restartedAt := app.Clock.Now()
	summary := app.ReconcileOnce()
	// 对账已观察到端口不在监听、重建尚未完成：界面必须明确告知端口暂时不可用（FR-028）。
	_, body := app.Get("/")
	if !strings.Contains(body, "端口暂时不可用") || !strings.Contains(body, "自动重建") {
		t.Fatalf("dashboard does not explain the restart window: %s", body)
	}
	_, body = app.Get("/users/" + active.User.ID.String())
	if !strings.Contains(body, "暂时不可用") {
		t.Fatalf("user detail does not explain the restart window: %s", body)
	}
	if !summary.Reconnected || summary.Drift != 1 {
		t.Fatalf("reconcile summary = %#v", summary)
	}
	_, _, next := opState(t, app, pending.Allocation.ID)
	if next.After(app.Clock.Now()) {
		app.Clock.Set(next)
	}
	app.Drain()
	// 存在 = 专属入站在监听且入站内含该用户的受管客户端。
	present := func(record ports.UserRecord) bool {
		tag := record.Inbound.Inbound.InboundTag
		if _, ok := app.Adapter.Inbounds[tag]; !ok {
			return false
		}
		_, ok := app.Adapter.Users[tag][record.Identity.StatisticsID]
		return ok
	}
	if !present(active) || present(disabled) || present(exceeded) || present(deleted) || !present(pending) {
		t.Fatalf("presence after recovery active=%v disabled=%v exceeded=%v deleted=%v pending=%v", present(active), present(disabled), present(exceeded), present(deleted), present(pending))
	}
	// 面板只处理自己命名空间内的入站：运维入站保留，孤儿入站被清理。
	if _, ok := app.Adapter.Inbounds["operator-inbound"]; !ok {
		t.Fatal("operator inbound outside the panel namespace was removed")
	}
	if _, ok := app.Adapter.Inbounds[domain.NamespacePrefix+"orphan"]; ok {
		t.Fatal("orphan inbound inside the panel namespace was not removed")
	}
	if app.Adapter.Users[pending.Inbound.Inbound.InboundTag][pending.Identity.StatisticsID].CredentialVersion != 2 {
		t.Fatal("pending rotation did not complete with the new credential")
	}
	// 每个用户各自一条入站、各自一个端口，重启后按原端口重建。
	assigned := map[int]string{}
	for _, record := range []ports.UserRecord{active, pending} {
		current := app.User(record.User.ID)
		port := current.Inbound.Inbound.Port
		if other, clash := assigned[port]; clash {
			t.Fatalf("port %d shared by %s and %s", port, other, current.User.DisplayName)
		}
		assigned[port] = current.User.DisplayName
		if !app.Listening(port) {
			t.Fatalf("port %d is not listening after recovery", port)
		}
	}
	// SC-006：一次对账 + 一次同步内按原端口收敛，用时远小于 60 秒的验收上界。
	if elapsed := app.Clock.Now().Sub(restartedAt); elapsed > 60*time.Second {
		t.Fatalf("convergence took %s, longer than the 60s bound", elapsed)
	}

	// 重启后计数回落：无负流量、无重复计量，事件可见。
	before := app.User(active.User.ID)
	app.SetTraffic(active, 100, 100)
	app.Collect()
	after := app.User(active.User.ID)
	if after.Cycle.AccountedUplinkBytes != before.Cycle.AccountedUplinkBytes+100 || after.Cycle.AccountedUplinkBytes < before.Cycle.AccountedUplinkBytes {
		t.Fatalf("restart accounting before=%d after=%d", before.Cycle.AccountedUplinkBytes, after.Cycle.AccountedUplinkBytes)
	}
	_, body = app.Get("/users/" + active.User.ID.String())
	if !strings.Contains(body, "节点已重启") {
		t.Fatalf("restart event not shown: %s", body)
	}
	if strings.Contains(body, "暂时不可用") {
		t.Fatalf("the restart notice is still shown after convergence: %s", body)
	}
	// 不确定的变更结果：超时但已生效 → 读后写确认，不重复添加。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: true, ExpectedRevision: 1, RequestID: testsupport.NewID(t), ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	// 停用移除的是整条入站，因此重新启用走的是 create_inbound。
	adds := countCalls(app, "create_inbound")
	app.Adapter.Failures["create_inbound"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorDeadlineExceeded, Operation: "create_inbound", Retryable: true, SafeSummary: "timed out"}, Applied: true}}
	app.Drain()
	if countCalls(app, "create_inbound") != adds+1 || !present(disabled) || app.User(disabled.User.ID).Allocation.ProjectionState != domain.ProjectionPresent {
		t.Fatalf("uncertain create: calls=%d present=%v", countCalls(app, "create_inbound")-adds, present(disabled))
	}
	// 应用在提交后、确认前重启：租约到期后从同一操作恢复到相同结果。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: false, ExpectedRevision: 2, RequestID: testsupport.NewID(t), ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	if work, err := app.Store.LeaseDueSync(context.Background(), "crashed-instance", app.Clock.Now(), time.Second); err != nil || work == nil {
		t.Fatalf("lease = %#v, %v", work, err)
	}
	app.Clock.Advance(2 * time.Second)
	app.Drain()
	if present(disabled) {
		t.Fatal("operation leased by a crashed instance was not resumed")
	}
	// 审计筛选无敏感信息。
	_, body = app.Get("/audit?user=" + pending.User.ID.String())
	if !strings.Contains(body, "轮换凭证") || strings.Contains(body, "ss://") || strings.Contains(body, app.Password) {
		t.Fatalf("audit body=%s", body)
	}
	_, body = app.Get("/")
	if !strings.Contains(body, "健康") || strings.Contains(body, "持续不同步") {
		t.Fatalf("dashboard after recovery body=%s", body)
	}
}

func opState(t *testing.T, app *testsupport.App, allocationID domain.ID) (string, int, time.Time) {
	t.Helper()
	var state string
	var attempts int
	var next int64
	if err := app.Store.DB().Read.QueryRow(`SELECT state,attempt_count,next_attempt_at FROM synchronization_operations WHERE allocation_id=? ORDER BY desired_revision DESC LIMIT 1`,
		allocationID.String()).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	return state, attempts, time.UnixMilli(next).UTC()
}

func countCalls(app *testsupport.App, operation string) int {
	count := 0
	for _, call := range app.Adapter.Calls {
		if call.Operation == operation {
			count++
		}
	}
	return count
}
