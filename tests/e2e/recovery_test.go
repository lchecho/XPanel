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
	profileID := app.RegisterCompatibleProfile("Primary")
	limit := int64(1 << 20)
	active := app.CreateUser("Active", profileID, nil)
	disabled := app.CreateUser("Disabled", profileID, nil)
	exceeded := app.CreateUser("Exceeded", profileID, &limit)
	deleted := app.CreateUser("Deleted", profileID, nil)
	pending := app.CreateUser("Pending", profileID, nil)
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
	// Xray 重启：动态用户丢失、bootstrap 与外部身份保留、boot epoch 变化。
	app.Clock.Advance(time.Minute)
	app.Adapter.Restart()
	app.Adapter.Users["managed"] = map[string]ports.RemoteUser{
		"bootstrap":       {StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"},
		"operator-static": {StatisticsID: "operator-static", Present: true, Kind: "external"},
	}
	app.Adapter.Available = true
	summary := app.ReconcileOnce()
	if !summary.Reconnected || summary.Drift != 1 {
		t.Fatalf("reconcile summary = %#v", summary)
	}
	_, _, next := opState(t, app, pending.Allocation.ID)
	if next.After(app.Clock.Now()) {
		app.Clock.Set(next)
	}
	app.Drain()
	present := func(record ports.UserRecord) bool {
		_, ok := app.Adapter.Users["managed"][record.Identity.StatisticsID]
		return ok
	}
	if !present(active) || present(disabled) || present(exceeded) || present(deleted) || !present(pending) {
		t.Fatalf("presence after recovery active=%v disabled=%v exceeded=%v deleted=%v pending=%v", present(active), present(disabled), present(exceeded), present(deleted), present(pending))
	}
	for _, id := range []string{"bootstrap", "operator-static"} {
		if _, ok := app.Adapter.Users["managed"][id]; !ok {
			t.Fatalf("%s identity was touched", id)
		}
	}
	if app.Adapter.Users["managed"][pending.Identity.StatisticsID].CredentialVersion != 2 {
		t.Fatal("pending rotation did not complete with the new credential")
	}
	// 重启后计数回落：无负流量、无重复计量，事件可见。
	before := app.User(active.User.ID)
	app.SetTraffic(active, 100, 100)
	app.Collect()
	after := app.User(active.User.ID)
	if after.Cycle.AccountedUplinkBytes != before.Cycle.AccountedUplinkBytes+100 || after.Cycle.AccountedUplinkBytes < before.Cycle.AccountedUplinkBytes {
		t.Fatalf("restart accounting before=%d after=%d", before.Cycle.AccountedUplinkBytes, after.Cycle.AccountedUplinkBytes)
	}
	_, body := app.Get("/users/" + active.User.ID.String())
	if !strings.Contains(body, "节点已重启") {
		t.Fatalf("restart event not shown: %s", body)
	}
	// 不确定的变更结果：超时但已生效 → 读后写确认，不重复添加。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: true, ExpectedRevision: 1, RequestID: testsupport.NewID(t), ActorID: actor}); err != nil {
		t.Fatal(err)
	}
	adds := countCalls(app, "add_user")
	app.Adapter.Failures["add_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorDeadlineExceeded, Operation: "add_user", Retryable: true, SafeSummary: "timed out"}, Applied: true}}
	app.Drain()
	if countCalls(app, "add_user") != adds+1 || !present(disabled) || app.User(disabled.User.ID).Allocation.ProjectionState != domain.ProjectionPresent {
		t.Fatalf("uncertain add: calls=%d present=%v", countCalls(app, "add_user")-adds, present(disabled))
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
