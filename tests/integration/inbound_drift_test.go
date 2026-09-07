package integration

import (
	"context"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// SC-014：面板命名空间内没有面板记录的入站被移除并释放端口且留下审计；
// 命名空间之外的运维自有入站在全流程中保持不动，也不计入面板统计（FR-031）。
func TestOrphanPanelInboundsAreRemovedAndOperatorInboundsAreUntouched(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	livePort := record.Inbound.Inbound.Port

	// 运维自有入站（命名空间之外）与两条面板命名空间内的孤儿入站。
	app.Adapter.AddExternalInbound("operator-inbound", 45000)
	orphanA, orphanB := domain.NamespacePrefix+"orphan-a", domain.NamespacePrefix+"orphan-b"
	app.Adapter.AddOrphanInbound(orphanA, 30090)
	app.Adapter.AddOrphanInbound(orphanB, 30091)

	summary := app.ReconcileOnce()
	if summary.RemovedUnknown != 2 {
		t.Fatalf("reconcile summary = %#v, want 2 unknown inbounds queued", summary)
	}
	app.Drain()

	for _, tag := range []string{orphanA, orphanB} {
		if _, present := app.Adapter.Inbounds[tag]; present {
			t.Fatalf("orphan inbound %s was not removed", tag)
		}
	}
	if app.Listening(30090) || app.Listening(30091) {
		t.Fatal("orphan ports were not released")
	}
	// 运维入站与真实用户都不受影响。
	if _, kept := app.Adapter.Inbounds["operator-inbound"]; !kept || !app.Listening(45000) {
		t.Fatal("the operator inbound outside the panel namespace was disturbed")
	}
	if !app.Listening(livePort) || app.User(record.User.ID).Allocation.PendingSync() {
		t.Fatal("the managed user was disturbed by drift cleanup")
	}

	// 审计：两次漂移清理都留痕，摘要不含密钥。
	var audits int
	if err := app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result='succeeded'`,
		domain.ActionReconcileRemovedUnknown).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("drift audits = %d, %v", audits, err)
	}
	var leaks int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE safe_summary LIKE '%ss://%' OR safe_summary LIKE '%=%'`).Scan(&leaks)
	if leaks != 0 {
		t.Fatalf("drift audit summaries look like they carry key material: %d", leaks)
	}

	// 再跑一轮对账不得产生新的漂移意图（清理是收敛的）。
	if again := app.ReconcileOnce(); again.RemovedUnknown != 0 || again.Drift != 0 {
		t.Fatalf("second reconcile produced work: %#v", again)
	}
}

// 面板命名空间内、且属于已删除用户的入站在重启后不得被重建；其端口已经回到池中可被再次分配。
func TestDeletedUsersInboundIsNotRebuiltAfterRestart(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Gone", templateID, nil)
	app.Drain()
	port := record.Inbound.Inbound.Port
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: record.User.ID,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()

	app.Adapter.Restart()
	app.ReconcileOnce()
	app.Drain()
	if app.Listening(port) {
		t.Fatalf("port %d of a deleted user was rebuilt after the restart", port)
	}
	successor := app.CreateUser("Successor", templateID, nil)
	app.Drain()
	if successor.Inbound.Inbound.Port != port {
		t.Fatalf("released port %d was not reassigned: got %d", port, successor.Inbound.Inbound.Port)
	}
	if !app.Listening(port) {
		t.Fatalf("reassigned port %d is not listening", port)
	}
}
