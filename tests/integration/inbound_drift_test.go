package integration

import (
	"context"
	"testing"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
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

// FR-031：面板命名空间内的孤立入站必须能被清理，即使此刻一个入站模板都没有、
// 或全部模板都已归档——它们不归属任何模板，意图以「无归属」持久化后照样执行。
func TestOrphanInboundsAreRemovedWithoutAnyOwningTemplate(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		setup func(t *testing.T, app *testsupport.App)
	}{
		{name: "no templates at all", setup: func(*testing.T, *testsupport.App) {}},
		{name: "every template archived", setup: func(t *testing.T, app *testsupport.App) {
			templateID := app.RegisterCompatibleTemplate("Retired")
			record, err := app.Store.Template(context.Background(), templateID)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Store.ArchiveTemplate(context.Background(), templateID, record.Template.Revision, app.Clock.Now()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app := testsupport.New(t)
			scenario.setup(t, app)
			orphan := domain.NamespacePrefix + "homeless"
			app.Adapter.AddOrphanInbound(orphan, 30800)
			app.Adapter.AddExternalInbound("operator-inbound", 45000)

			if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
				t.Fatalf("reconcile summary = %#v, want the orphan queued", summary)
			}
			app.Drain()
			if _, present := app.Adapter.Inbounds[orphan]; present {
				t.Fatal("orphan inbound was not removed without an owning template")
			}
			if app.Listening(30800) {
				t.Fatal("the orphan port was not released")
			}
			if _, kept := app.Adapter.Inbounds["operator-inbound"]; !kept {
				t.Fatal("an inbound outside the panel namespace was removed")
			}
			var audits int
			_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result='succeeded'`,
				domain.ActionReconcileRemovedUnknown).Scan(&audits)
			if audits != 1 {
				t.Fatalf("drift audits = %d", audits)
			}
			// 收敛：再跑一轮不产生新意图。
			if again := app.ReconcileOnce(); again.RemovedUnknown != 0 || again.ConfirmedAbsent != 0 {
				t.Fatalf("second reconcile produced work: %#v", again)
			}
		})
	}
}

// 无归属的移除意图永久失败后，协调器必须重新排队确认，否则孤立入站会永远占着端口。
func TestPermanentlyFailedOrphanRemovalWithoutTemplateIsRequeued(t *testing.T) {
	app := testsupport.New(t)
	orphan := domain.NamespacePrefix + "stubborn"
	app.Adapter.AddOrphanInbound(orphan, 30801)
	app.ReconcileOnce()
	app.Adapter.Failures["remove_inbound"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected,
		Operation: "remove_inbound", Retryable: false, SafeSummary: "rejected by Xray"}}}
	app.Drain()
	var state string
	var owner *string
	_ = app.Store.DB().Read.QueryRow(`SELECT state,template_id FROM drift_removals WHERE inbound_tag=?`, orphan).Scan(&state, &owner)
	if state != string(domain.SyncPermanentFailed) || owner != nil {
		t.Fatalf("drift removal state=%s template=%v, want permanent_failed without an owner", state, owner)
	}
	// 目标仍在：协调器重新排队移除。
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile after failure = %#v", summary)
	}
	app.Drain()
	if _, present := app.Adapter.Inbounds[orphan]; present {
		t.Fatal("orphan inbound survived the retry")
	}
	// 目标已不在时，永久失败的意图也必须被重新排队并确认，而不是永远留在库里。
	app.Adapter.AddOrphanInbound(orphan, 30802)
	app.Adapter.Failures["remove_inbound"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected,
		Operation: "remove_inbound", Retryable: false, SafeSummary: "rejected by Xray"}}}
	app.ReconcileOnce()
	app.Drain()
	delete(app.Adapter.Inbounds, orphan)
	delete(app.Adapter.Users, orphan)
	if summary := app.ReconcileOnce(); summary.ConfirmedAbsent != 1 {
		t.Fatalf("absence confirmation not queued: %#v", summary)
	}
	app.Drain()
	var open int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE state IN ('pending','leased','retry_wait')`).Scan(&open)
	if open != 0 {
		t.Fatalf("open drift removals = %d", open)
	}
}
