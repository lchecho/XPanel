package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

// T089 / FR-005：兼容结论只对做出它的那个 Xray 进程有效。节点重启后能力证据被置回待验证并立即重跑门禁；
// 证据尚未刷新到当前世代之前，新建用户必须被拒绝；既有用户则照常按原端口恢复。
func TestCapabilityEvidenceIsBoundToTheRunningXrayGeneration(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	existing := app.CreateUser("Existing", templateID, nil)
	app.Drain()
	port := existing.Inbound.Inbound.Port
	original, err := app.Store.Template(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Template.ValidatedBootEpoch == "" {
		t.Fatal("the template did not record the boot epoch it was validated against")
	}

	// 重启：boot epoch 变化，旧结论不再适用于新进程；协调器置回待验证并立即重跑门禁。
	app.Clock.Advance(time.Minute)
	app.Adapter.Restart()
	summary := app.ReconcileOnce()
	if summary.Revalidated != 1 {
		t.Fatalf("reconcile summary = %#v, want the stale template revalidated", summary)
	}
	instance, _ := app.Store.ManagedInstance(context.Background())
	refreshed, _ := app.Store.Template(context.Background(), templateID)
	if refreshed.Template.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("template was not revalidated against the new generation: %#v", refreshed.Template)
	}
	if refreshed.Template.ValidatedBootEpoch != instance.BootEpoch ||
		refreshed.Template.ValidatedBootEpoch == original.Template.ValidatedBootEpoch {
		t.Fatalf("evidence still points at the old generation: %q (instance %q, was %q)",
			refreshed.Template.ValidatedBootEpoch, instance.BootEpoch, original.Template.ValidatedBootEpoch)
	}

	// 既有用户按原端口恢复，重启不影响他们。
	app.Drain()
	current := app.User(existing.User.ID)
	if current.Inbound.Inbound.Port != port || !app.Listening(port) || current.Allocation.PendingSync() {
		t.Fatalf("existing user was not restored: %#v", current.Allocation)
	}

	// 证据尚未刷新到当前世代（例如重新验证还没跑完）时，新建用户必须被拒绝且不留部分状态。
	if _, err := app.Store.DB().Write.Exec(`UPDATE inbound_templates SET validated_boot_epoch=NULL WHERE id=?`,
		templateID.String()); err != nil {
		t.Fatal(err)
	}
	before := len(app.ListUsers())
	_, _, err = app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fresh",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var invalid *domain.InvalidStateError
	if !errors.As(err, &invalid) {
		t.Fatalf("creation against stale capability evidence = %T %v", err, err)
	}
	if after := len(app.ListUsers()); after != before || app.Drain() != 0 {
		t.Fatal("the rejected creation left partial state")
	}

	// 对当前世代重跑门禁通过后，新建恢复正常。
	if err := app.Validator.ValidateNow(context.Background(), templateID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fresh",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatalf("creation after revalidation: %v", err)
	}
	app.Drain()

	// 二次对账不再重复置回待验证（证据已经绑定当前世代）。
	if again := app.ReconcileOnce(); again.Revalidated != 0 {
		t.Fatalf("second reconcile invalidated again: %#v", again)
	}
}

// 重启后节点能力真的变了（例如 policy 被去掉）：重跑门禁必须判为不兼容，新建用户持续被拒绝。
func TestCapabilityRevalidationCatchesANodeThatLostItsStatsPolicy(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	app.Clock.Advance(time.Minute)
	app.Adapter.Restart()
	// 新进程缺少用户级统计能力。
	app.Adapter.Templates[templateID.String()] = ports.TemplateCapabilities{InboundCreatable: true, InboundRemovable: true,
		ProtocolSupported: true, MethodSupported: true, MultiUserSupported: true, TrafficAccounted: false,
		CompatibilityReason: "node does not report per-user traffic counters; enable statsUserUplink and statsUserDownlink"}

	app.ReconcileOnce()
	if err := app.Validator.ValidateNow(context.Background(), templateID); err != nil {
		t.Fatal(err)
	}
	record, _ := app.Store.Template(context.Background(), templateID)
	if record.Template.Compatibility != domain.CompatibilityIncompatible || record.Template.ValidatedBootEpoch != "" {
		t.Fatalf("template after revalidation against a degraded node = %#v", record.Template)
	}
	_, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fresh",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var invalid *domain.InvalidStateError
	if !errors.As(err, &invalid) {
		t.Fatalf("creation against an incompatible node = %T %v", err, err)
	}
}
