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
	if original.Template.ValidatedGeneration <= 0 {
		t.Fatal("the template did not record the capability generation it was validated against")
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
	if refreshed.Template.ValidatedGeneration != instance.CapabilityGeneration ||
		refreshed.Template.ValidatedGeneration == original.Template.ValidatedGeneration {
		t.Fatalf("evidence still points at the old generation: %d (instance %d, was %d)",
			refreshed.Template.ValidatedGeneration, instance.CapabilityGeneration, original.Template.ValidatedGeneration)
	}

	// 既有用户按原端口恢复，重启不影响他们。
	app.Drain()
	current := app.User(existing.User.ID)
	if current.Inbound.Inbound.Port != port || !app.Listening(port) || current.Allocation.PendingSync() {
		t.Fatalf("existing user was not restored: %#v", current.Allocation)
	}

	// 证据尚未刷新到当前世代（例如重新验证还没跑完）时，新建用户必须被拒绝且不留部分状态。
	if _, err := app.Store.DB().Write.Exec(`UPDATE inbound_templates SET validated_generation=NULL WHERE id=?`,
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
	if record.Template.Compatibility != domain.CompatibilityIncompatible || record.Template.ValidatedGeneration != 0 {
		t.Fatalf("template after revalidation against a degraded node = %#v", record.Template)
	}
	_, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fresh",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var invalid *domain.InvalidStateError
	if !errors.As(err, &invalid) {
		t.Fatalf("creation against an incompatible node = %T %v", err, err)
	}
}

// T094：能力世代只在**已确认的**重启后前进。boot epoch 由 uint32 整秒 uptime 推算，
// 同一个 Xray 进程相邻两次探测就会有 ±1 秒抖动；若用字符串精确不等判定重启，
// 模板会在没有任何重启的情况下被反复置回待验证。
func TestQuantizationJitterDoesNotInvalidateCapabilityEvidence(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	original, _ := app.Store.Template(context.Background(), templateID)

	// 连续多轮对账，每轮让 boot epoch 抖动 ±1 秒（同一进程的量化误差，不是重启）。
	base := app.Adapter.BootEpoch
	for i, offset := range []time.Duration{time.Second, -time.Second, time.Second, 0, -time.Second} {
		app.Adapter.BootEpoch = base.Add(offset)
		app.Clock.Advance(15 * time.Second)
		if summary := app.ReconcileOnce(); summary.Revalidated != 0 {
			t.Fatalf("round %d: quantization jitter (%s) invalidated the evidence: %#v", i, offset, summary)
		}
	}
	after, _ := app.Store.Template(context.Background(), templateID)
	if after.Template.Compatibility != domain.CompatibilityCompatible ||
		after.Template.ValidatedGeneration != original.Template.ValidatedGeneration {
		t.Fatalf("template drifted under jitter: %#v (was generation %d)", after.Template, original.Template.ValidatedGeneration)
	}
	instance, _ := app.Store.ManagedInstance(context.Background())
	if instance.CapabilityGeneration != original.Template.ValidatedGeneration {
		t.Fatalf("generation advanced without a restart: %d", instance.CapabilityGeneration)
	}

	// 真正的重启（远超一秒容差）才让世代前进。
	app.Clock.Advance(time.Minute)
	app.Adapter.Restart()
	if summary := app.ReconcileOnce(); summary.Revalidated != 1 {
		t.Fatalf("a real restart did not invalidate the evidence: %#v", summary)
	}
	restarted, _ := app.Store.ManagedInstance(context.Background())
	if restarted.CapabilityGeneration <= original.Template.ValidatedGeneration {
		t.Fatalf("generation did not advance across a real restart: %d", restarted.CapabilityGeneration)
	}
}

// T098：协调器明确观察到的断线重连必须触发保守重新验证——重连期间节点可能已经重启并换了配置，
// 面板对此一无所知，不能假设它还是原来那个进程。
func TestObservedReconnectTriggersConservativeRevalidation(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	before, _ := app.Store.Template(context.Background(), templateID)
	beforeInstance, _ := app.Store.ManagedInstance(context.Background())

	app.Adapter.Available = false
	if _, err := app.Reconcile.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("reconcile succeeded while Xray is unavailable")
	}
	app.Adapter.Available = true
	app.Clock.Advance(30 * time.Second)
	summary := app.ReconcileOnce()
	if !summary.Reconnected {
		t.Fatalf("reconnect was not detected: %#v", summary)
	}
	if summary.Revalidated != 1 {
		t.Fatalf("an observed reconnect did not trigger revalidation: %#v", summary)
	}
	instance, _ := app.Store.ManagedInstance(context.Background())
	if instance.CapabilityGeneration <= beforeInstance.CapabilityGeneration {
		t.Fatalf("generation did not advance across a reconnect: %d → %d",
			beforeInstance.CapabilityGeneration, instance.CapabilityGeneration)
	}
	// 每个独立的重连事件只推进一次：后续对账（没有新的重连）不得再推进。
	for i := 0; i < 2; i++ {
		app.Clock.Advance(15 * time.Second)
		if again := app.ReconcileOnce(); again.Revalidated != 0 || again.Reconnected {
			t.Fatalf("round %d treated a steady connection as a new reconnect: %#v", i, again)
		}
		current, _ := app.Store.ManagedInstance(context.Background())
		if current.CapabilityGeneration != instance.CapabilityGeneration {
			t.Fatalf("round %d advanced the generation without a new reconnect: %d → %d", i,
				instance.CapabilityGeneration, current.CapabilityGeneration)
		}
	}
	// 重新验证在同一轮内完成，模板重新绑定到新世代。
	after, _ := app.Store.Template(context.Background(), templateID)
	if after.Template.Compatibility != domain.CompatibilityCompatible ||
		after.Template.ValidatedGeneration != instance.CapabilityGeneration ||
		after.Template.ValidatedGeneration == before.Template.ValidatedGeneration {
		t.Fatalf("template evidence after the reconnect = %#v (instance generation %d)",
			after.Template, instance.CapabilityGeneration)
	}
}

// T098：uptime 回落是不会被整秒量化吞掉的重启信号——即使前后 boot epoch 完全相同，
// 「快速重启」也必须让旧证据立即失效。
func TestSameEpochRestartIsCaughtByVanishedInbounds(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	app.CreateUser("Alice", templateID, nil)
	app.Drain()
	// 先跑几轮让锚点与 uptime 稳定下来。
	for i := 0; i < 3; i++ {
		app.Clock.Advance(15 * time.Second)
		if summary := app.ReconcileOnce(); summary.Revalidated != 0 {
			t.Fatalf("round %d invalidated without a restart: %#v", i, summary)
		}
	}
	before, _ := app.Store.Template(context.Background(), templateID)

	// 重启但把 boot epoch 与 uptime 都保持原样：epoch 差值和 uptime 单调性两个信号都被吞掉，
	// 只剩「本应监听的面板入站全部消失」能暴露它。
	epoch := app.Adapter.BootEpoch
	app.Adapter.Restart()
	app.Adapter.BootEpoch = epoch
	instanceBefore, _ := app.Store.ManagedInstance(context.Background())
	if summary := app.ReconcileOnce(); summary.Revalidated != 1 {
		t.Fatalf("a same-epoch restart was not detected: %#v", summary)
	}
	instance, _ := app.Store.ManagedInstance(context.Background())
	if instance.CapabilityGeneration <= instanceBefore.CapabilityGeneration {
		t.Fatalf("generation did not advance across a same-epoch restart: %d → %d",
			instanceBefore.CapabilityGeneration, instance.CapabilityGeneration)
	}
	after, _ := app.Store.Template(context.Background(), templateID)
	if after.Template.ValidatedGeneration == before.Template.ValidatedGeneration {
		t.Fatalf("template evidence survived a same-epoch restart: %#v", after.Template)
	}

	// T099：这是一次**边沿**事件。入站还没被重建（这里刻意不 drain）时继续对账，
	// 世代必须保持不变，刚跑完的能力门禁不能被反复作废。
	for i := 0; i < 2; i++ {
		app.Clock.Advance(15 * time.Second)
		summary := app.ReconcileOnce()
		if summary.Revalidated != 0 {
			t.Fatalf("round %d re-invalidated while the inbounds were still missing: %#v", i, summary)
		}
		current, _ := app.Store.ManagedInstance(context.Background())
		if current.CapabilityGeneration != instance.CapabilityGeneration {
			t.Fatalf("round %d advanced the generation again: %d → %d", i,
				instance.CapabilityGeneration, current.CapabilityGeneration)
		}
	}

	// 入站恢复之后再次整体消失，是一次新的事件，必须再次推进。
	app.Drain()
	if summary := app.ReconcileOnce(); summary.Revalidated != 0 {
		t.Fatalf("reconcile after recovery invalidated again: %#v", summary)
	}
	recovered, _ := app.Store.ManagedInstance(context.Background())
	epoch = app.Adapter.BootEpoch
	app.Adapter.Restart()
	app.Adapter.BootEpoch = epoch
	if summary := app.ReconcileOnce(); summary.Revalidated != 1 {
		t.Fatalf("a second disappearance after recovery was not treated as a new event: %#v", summary)
	}
	final, _ := app.Store.ManagedInstance(context.Background())
	if final.CapabilityGeneration <= recovered.CapabilityGeneration {
		t.Fatalf("generation did not advance on the second disappearance: %d → %d",
			recovered.CapabilityGeneration, final.CapabilityGeneration)
	}
}
