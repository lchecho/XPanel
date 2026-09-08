package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

// T086 / FR-019：专属入站里只应有该用户的期望身份。任何其它客户端都是漂移，
// 包括没有 xpanel- 前缀的外部身份——它同样能用未知密钥经这个端口出网。
func TestForeignIdentityInsideAPanelInboundIsRemoved(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	bystander := app.CreateUser("Bob", templateID, nil)
	app.Drain()
	tag, port := record.Inbound.Inbound.InboundTag, record.Inbound.Inbound.Port
	bystanderTag := bystander.Inbound.Inbound.InboundTag

	// 两类未知身份：命名空间之外的和命名空间之内的，都必须被清理。
	app.Adapter.InjectClient(tag, "intruder-without-prefix")
	app.Adapter.InjectClient(tag, domain.NamespacePrefix+"intruder-with-prefix")
	app.Adapter.AddExternalInbound("operator-inbound", 45000)

	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 2 {
		t.Fatalf("reconcile summary = %#v, want both unknown identities queued", summary)
	}
	app.Drain()

	clients := app.Adapter.Users[tag]
	if len(clients) != 1 {
		t.Fatalf("panel inbound holds %d clients after cleanup: %#v", len(clients), clients)
	}
	if _, ok := clients[record.Identity.StatisticsID]; !ok {
		t.Fatalf("the expected identity was removed instead of the intruders: %#v", clients)
	}
	if !app.Listening(port) || app.User(record.User.ID).Allocation.PendingSync() {
		t.Fatal("cleanup disturbed the user's own inbound")
	}
	// 其它入站与运维自有入站不受影响。
	if len(app.Adapter.Users[bystanderTag]) != 1 || !app.Listening(bystander.Inbound.Inbound.Port) {
		t.Fatal("cleanup disturbed another user's inbound")
	}
	if _, kept := app.Adapter.Inbounds["operator-inbound"]; !kept {
		t.Fatal("an inbound outside the panel namespace was touched")
	}
	// 收敛：再跑一轮不产生新的漂移意图。
	if again := app.ReconcileOnce(); again.RemovedUnknown != 0 {
		t.Fatalf("second reconcile produced more drift: %#v", again)
	}
}

// 入站里只剩未知身份（期望身份已经不见）时，任何顺序都不得制造「没有受管客户端」的入站：
// 正确结果是期望身份被恢复、未知身份被清掉。
func TestPanelInboundLeftWithOnlyUnknownIdentitiesIsRestored(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	tag, port := record.Inbound.Inbound.InboundTag, record.Inbound.Inbound.Port

	// 期望身份消失，只剩一个未知身份守着这条入站。
	delete(app.Adapter.Users[tag], record.Identity.StatisticsID)
	app.Adapter.InjectClient(tag, "intruder-without-prefix")

	// 重建入站与清理漂移是两条独立意图；坏入站被补偿移除后重建走的是有界退避重试，
	// 因此需要把时钟推到下一次重试时刻。
	for i := 0; i < 4; i++ {
		app.ReconcileOnce()
		app.Drain()
		var next sql.NullInt64
		_ = app.Store.DB().Read.QueryRow(`SELECT MIN(CASE WHEN state='retry_wait' THEN next_attempt_at WHEN state='leased' THEN lease_expires_at END)
            FROM synchronization_operations WHERE state IN ('retry_wait','leased')`).Scan(&next)
		if next.Valid && next.Int64 > 0 {
			if at := time.UnixMilli(next.Int64).UTC(); at.After(app.Clock.Now()) {
				app.Clock.Set(at)
			}
		}
	}

	clients := app.Adapter.Users[tag]
	if _, ok := clients[record.Identity.StatisticsID]; !ok {
		t.Fatalf("the expected identity was not restored: %#v", clients)
	}
	if _, intruder := clients["intruder-without-prefix"]; intruder {
		t.Fatalf("the unknown identity survived: %#v", clients)
	}
	if len(clients) != 1 {
		t.Fatalf("panel inbound holds %d clients: %#v", len(clients), clients)
	}
	if !app.Listening(port) {
		t.Fatalf("port %d is not listening after recovery", port)
	}
	if current := app.User(record.User.ID); current.Allocation.PendingSync() {
		t.Fatalf("allocation still pending: %#v", current.Allocation)
	}
}

// 轮换进行中，过渡身份不得被当成漂移清掉；轮换收敛后它必须消失。
func TestRotationTransitionIdentityIsExemptOnlyWhileTheIntentIsOpen(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	tag := record.Inbound.Inbound.InboundTag
	safety := domain.RotationSafetyID(record.Identity.StatisticsID)

	// 轮换意图已提交但尚未执行：过渡身份此刻是合法的中间状态。
	if _, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: record.User.ID,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Adapter.InjectClient(tag, safety)
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 0 {
		t.Fatalf("the transition identity was treated as drift while rotating: %#v", summary)
	}

	// 轮换收敛后同一个身份不再豁免：它必须被清理。
	app.Drain()
	if _, leftover := app.Adapter.Users[tag][safety]; leftover {
		t.Fatal("the synchronizer left the transition identity behind")
	}
	app.Adapter.InjectClient(tag, safety)
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("a leftover transition identity outside an open rotation was not queued: %#v", summary)
	}
	app.Drain()
	if _, leftover := app.Adapter.Users[tag][safety]; leftover {
		t.Fatal("the leftover transition identity was not removed")
	}
	if len(app.Adapter.Users[tag]) != 1 {
		t.Fatalf("panel inbound holds %#v", app.Adapter.Users[tag])
	}
}

// T097：期望身份必须由调用方显式给出——缺少它时适配器直接拒绝，不能退化成「凭标签猜」。
func TestRemoveUserRequiresTheExplicitExpectedIdentity(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	tag := record.Inbound.Inbound.InboundTag
	app.Adapter.InjectClient(tag, "intruder-without-prefix")

	_, err := app.Adapter.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
		StatisticsID: "intruder-without-prefix"})
	var adapterErr *ports.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorInvalidArgument {
		t.Fatalf("remove_user without the expected identity = %v", err)
	}
	// 显式给出后同一次清理是允许的。
	if _, err := app.Adapter.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
		StatisticsID: "intruder-without-prefix", ExpectedStatisticsID: record.Identity.StatisticsID}); err != nil {
		t.Fatalf("cleaning the intruder with the expected identity given: %v", err)
	}
}

// T092：只有未完成的**轮换**意图才豁免过渡身份。普通的未完成意图（例如待同步的创建）
// 不得庇护遗留的过渡身份。
func TestOnlyAnOpenRotationExemptsTheTransitionIdentity(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	// 创建意图尚未同步：这是一条未完成的非轮换意图。
	tag := record.Inbound.Inbound.InboundTag
	safety := domain.RotationSafetyID(record.Identity.StatisticsID)
	app.Drain()
	app.Adapter.InjectClient(tag, safety)

	// 制造一条未完成的禁用意图（不推进 worker），它不得庇护遗留的过渡身份。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID,
		Enabled: false, ExpectedRevision: app.User(record.User.ID).User.Revision,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	var open int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=?
        AND state IN ('pending','leased','retry_wait')`, record.Allocation.ID.String()).Scan(&open)
	if open == 0 {
		t.Fatal("the fixture did not leave an open non-rotation operation")
	}
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("a leftover transition identity was protected by an unrelated open operation: %#v", summary)
	}
}

// T092：有效后继只认「期望身份 + 它的轮换过渡身份」。入站里剩下别的身份——无论有没有 xpanel- 前缀——
// 都不算「还有人」，移除期望身份必须被拒绝。
func TestOnlyTheExactExpectedOrTransitionIdentityCountsAsASurvivor(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	tag := record.Inbound.Inbound.InboundTag
	expected := record.Identity.StatisticsID

	for _, leftover := range []string{"intruder-without-prefix", domain.NamespacePrefix + "someone-else"} {
		app.Adapter.InjectClient(tag, leftover)
		_, err := app.Adapter.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
			StatisticsID: expected, ExpectedStatisticsID: expected})
		var adapterErr *ports.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorLastManagedClient {
			t.Fatalf("removing the expected identity while %q remains = %v", leftover, err)
		}
		// 清理未知身份是允许的：期望身份还在。
		if _, err := app.Adapter.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
			StatisticsID: leftover, ExpectedStatisticsID: expected}); err != nil {
			t.Fatalf("cleaning %q while the expected identity is present: %v", leftover, err)
		}
	}
	// 轮换过渡身份是唯一被认可的后继。
	app.Adapter.InjectClient(tag, domain.RotationSafetyID(expected))
	if _, err := app.Adapter.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
		StatisticsID: expected, ExpectedStatisticsID: expected}); err != nil {
		t.Fatalf("removing the expected identity while its transition identity remains: %v", err)
	}
	if len(app.Adapter.Users[tag]) != 1 {
		t.Fatalf("inbound holds %#v", app.Adapter.Users[tag])
	}
}
