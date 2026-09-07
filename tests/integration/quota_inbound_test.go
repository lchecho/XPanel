package integration

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// pendingPhase 读取某分配某原因下最新一条尚未完成的同步意图的阶段。
func pendingPhase(t *testing.T, app *testsupport.App, allocationID domain.ID, reason string) string {
	t.Helper()
	var phase string
	if err := app.Store.DB().Read.QueryRow(`SELECT phase FROM synchronization_operations WHERE allocation_id=? AND reason=?
        ORDER BY created_at DESC LIMIT 1`, allocationID.String(), reason).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	return phase
}

// 配额封禁作用在端口层面：超限移除整条入站使端口停止监听，新周期以同一端口重建（FR-019/SC-005）。
func TestQuotaBlockStopsThePortAndRestoreReusesIt(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	record := app.CreateUser("Blocked", templateID, &limit)
	app.Drain()
	port := record.Inbound.Inbound.Port
	if !app.Listening(port) {
		t.Fatalf("port %d is not listening before the quota is exceeded", port)
	}
	app.SetTraffic(record, uint64(limit), 0)
	if summary := app.Collect(); summary.Blocked != 1 {
		t.Fatalf("block summary = %#v", summary)
	}
	// 意图落库时阶段就是「移除入站」；同步完成后阶段会变成 done，所以在推进 worker 之前读取。
	if phase := pendingPhase(t, app, record.Allocation.ID, "quota_block"); phase != string(domain.SyncRemoveInbound) {
		t.Fatalf("quota block phase = %q", phase)
	}
	app.Drain()
	record = app.User(record.User.ID)
	if record.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatalf("user not blocked: %#v", record.Allocation)
	}
	if app.Listening(port) {
		t.Fatalf("port %d kept listening after the quota was exceeded", port)
	}
	// 停止访问必须是「移除整条入站」，而不是把入站留在原地只删客户端。
	tag := record.Inbound.Inbound.InboundTag
	if _, present := app.Adapter.Inbounds[tag]; present {
		t.Fatal("the dedicated inbound survived the quota block without a managed client")
	}

	// 新周期：以同一端口恢复监听，且不重新分配端口。
	app.Clock.Set(record.Cycle.EndsAt.Add(time.Second))
	if app.Rollover() != 1 {
		t.Fatal("expected exactly one boundary")
	}
	if phase := pendingPhase(t, app, record.Allocation.ID, "quota_restore"); phase != string(domain.SyncCreateInbound) {
		t.Fatalf("quota restore phase = %q", phase)
	}
	app.Drain()
	restored := app.User(record.User.ID)
	if restored.Inbound.Inbound.Port != port || !app.Listening(port) {
		t.Fatalf("restored port = %d want %d listening", restored.Inbound.Inbound.Port, port)
	}
	if restored.Allocation.QuotaState != domain.QuotaWithinLimit || restored.Allocation.PendingSync() {
		t.Fatalf("allocation after restore = %#v", restored.Allocation)
	}
	// 端口分配行自始至终没有被释放过：恢复复用的是原分配而不是新分配。
	var assignments int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM dedicated_inbounds WHERE allocation_id=?`, record.Allocation.ID.String()).Scan(&assignments)
	if assignments != 1 {
		t.Fatalf("port assignments for the allocation = %d, want 1", assignments)
	}
}

// 手动禁用的用户不因新周期恢复；配额恢复只作用于「仅因配额而不监听」的用户。
func TestCycleRestoreDoesNotReviveManuallyDisabledPorts(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	record := blockedUser(t, app, templateID, "Blocked", limit)
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: false,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	app.Clock.Set(record.Cycle.EndsAt.Add(time.Second))
	app.Rollover()
	app.Drain()
	if app.Listening(record.Inbound.Inbound.Port) {
		t.Fatalf("port %d came back for a manually disabled user", record.Inbound.Inbound.Port)
	}
	if current := app.User(record.User.ID); current.Allocation.PendingSync() {
		t.Fatalf("allocation left pending: %#v", current.Allocation)
	}
}

// 调低配额到低于当前用量：下一轮采集立即进入超限处理，端口停止监听。
func TestLoweringTheQuotaBelowUsageStopsThePort(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	large := int64(1 << 30)
	record := app.CreateUser("Heavy", templateID, &large)
	app.Drain()
	port := record.Inbound.Inbound.Port
	app.SetTraffic(record, 4<<20, 0)
	app.Collect()
	if !app.Listening(port) {
		t.Fatalf("port %d should still listen below the quota", port)
	}
	small := int64(1 << 20)
	if _, err := app.Users.UpdateUser(context.Background(), application.UpdateUserInput{ID: record.User.ID,
		DisplayName: record.User.DisplayName, LimitBytes: &small, ResetDay: 1, AdminEnabled: true,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Collect()
	app.Drain()
	if app.Listening(port) {
		t.Fatalf("port %d kept listening after the quota was lowered below usage", port)
	}
	if state := app.User(record.User.ID).Allocation.QuotaState; state != domain.QuotaExceeded {
		t.Fatalf("quota state = %s", state)
	}
}
