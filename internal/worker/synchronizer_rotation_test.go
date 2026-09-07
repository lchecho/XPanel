package worker

import (
	"context"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// rotate 触发一次凭证轮换并返回轮换后的记录（未同步）。
func (f *syncFixture) rotate(t *testing.T, record ports.UserRecord) ports.UserRecord {
	t.Helper()
	current, err := f.store.User(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.users.RotateCredential(context.Background(), application.LifecycleInput{ID: record.User.ID,
		ExpectedRevision: current.User.Revision, RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.User(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	return after
}

// assertNoEmptyInbound 断言 Xray 中不存在「面板命名空间内、却没有任何受管客户端」的入站。
//
// AI-LOCK：这是 FR-019 的不变量。任何持久阶段之间（每次 Drain 之后）都必须成立；
// 轮换在单个租约步骤内先删后加，空窗只存在于两次 RPC 之间，不得被持久化。
func (f *syncFixture) assertNoEmptyInbound(t *testing.T, step string) {
	t.Helper()
	for tag := range f.adapter.Inbounds {
		if !domain.IsPanelNamespace(tag) {
			continue
		}
		managed := 0
		for _, user := range f.adapter.Users[tag] {
			if user.Present && user.Kind == "managed" {
				managed++
			}
		}
		if managed == 0 {
			t.Fatalf("%s: panel inbound %s has no managed client", step, tag)
		}
	}
}

// 正常轮换：端口与入站标签不变、监听不中断、统计身份不变、旧凭证被新版本取代。
func TestRotationKeepsPortAndIdentityAndReplacesTheKey(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tag, port := f.tagOf(record), record.Inbound.Inbound.Port
	before := f.adapter.Users[tag][record.Identity.StatisticsID]

	rotated := f.rotate(t, record)
	inboundCalls := f.calls("create_inbound") + f.calls("remove_inbound")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after rotation")

	after, _ := f.store.User(context.Background(), record.User.ID)
	if after.Inbound.Inbound.Port != port || after.Inbound.Inbound.InboundTag != tag {
		t.Fatalf("rotation moved the inbound: %#v", after.Inbound.Inbound)
	}
	if after.Identity.StatisticsID != record.Identity.StatisticsID {
		t.Fatal("rotation changed the statistics identity")
	}
	// 端口不中断：轮换过程中没有任何入站创建或移除调用。
	if now := f.calls("create_inbound") + f.calls("remove_inbound"); now != inboundCalls {
		t.Fatalf("rotation touched the inbound lifecycle: %d extra calls", now-inboundCalls)
	}
	remote := f.adapter.Users[tag][record.Identity.StatisticsID]
	if remote.CredentialVersion != rotated.Allocation.DesiredCredentialVersion || remote.CredentialVersion == before.CredentialVersion {
		t.Fatalf("remote credential version = %d, before = %d, desired = %d", remote.CredentialVersion,
			before.CredentialVersion, rotated.Allocation.DesiredCredentialVersion)
	}
	if after.Credential.State != domain.CredentialActive || after.Allocation.PendingSync() {
		t.Fatalf("allocation after rotation = %#v credential=%s", after.Allocation, after.Credential.State)
	}
	// 旧凭证已销毁，无法再建立新连接。
	var live int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM access_credentials WHERE allocation_id=? AND version<? AND state!='destroyed'`,
		after.Allocation.ID.String(), after.Credential.Version).Scan(&live)
	if live != 0 {
		t.Fatalf("old credentials still usable: %d", live)
	}
}

// 轮换意图是单阶段的：不得持久化「已移除旧客户端、尚未加入新客户端」的中间阶段（FR-019）。
func TestRotationNeverPersistsAnEmptyInbound(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	rotated := f.rotate(t, record)
	var phase string
	if err := f.store.DB().Read.QueryRow(`SELECT phase FROM synchronization_operations WHERE allocation_id=? AND reason='rotate'`,
		rotated.Allocation.ID.String()).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != string(domain.SyncAddDesired) {
		t.Fatalf("rotation phase = %q, want a single add-desired phase", phase)
	}
	// 阶段推进能力已从 Store 移除：轮换不再有「移除旧凭证」这个可恢复的持久阶段。
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after rotation drain")
}

// 加回新凭证失败时必须补偿移除整条入站，而不是留下没有受管客户端的入站；重试按原端口重建。
func TestRotationCompensatesWhenTheNewCredentialCannotBeAdded(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tag, port := f.tagOf(record), record.Inbound.Inbound.Port
	f.rotate(t, record)

	// 不可重试的失败：新客户端加不进去。
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected,
		Operation: "add_user", Retryable: false, SafeSummary: "rejected"}}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after failed rotation")
	if _, present := f.adapter.Inbounds[tag]; present {
		t.Fatal("the inbound survived a failed rotation without a managed client")
	}
	if f.adapter.Listening(port) {
		t.Fatalf("port %d still listening after the compensating removal", port)
	}
	after, _ := f.store.User(context.Background(), record.User.ID)
	if after.Inbound.Inbound.ObservedPresent == nil || *after.Inbound.Inbound.ObservedPresent {
		t.Fatalf("the panel still reports the inbound as listening: %#v", after.Inbound.Inbound)
	}

	// 恢复后：协调器或重试按原端口整体重建，且带着期望凭证。
	delete(f.adapter.Failures, "add_user")
	_, _, next := f.operationState(t, record.Allocation.ID)
	if next.After(f.clock.Now()) {
		f.clock.Time = next
	}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	rebuilt, _ := f.store.User(context.Background(), record.User.ID)
	if rebuilt.Inbound.Inbound.Port != port || !f.adapter.Listening(port) {
		t.Fatalf("rebuilt port = %d want %d listening", rebuilt.Inbound.Inbound.Port, port)
	}
	remote := f.adapter.Users[tag][record.Identity.StatisticsID]
	if remote.CredentialVersion != rebuilt.Allocation.DesiredCredentialVersion {
		t.Fatalf("rebuilt credential version = %d want %d", remote.CredentialVersion, rebuilt.Allocation.DesiredCredentialVersion)
	}
	f.assertNoEmptyInbound(t, "after rebuild")
}

// 进程在两次 RPC 之间崩溃（旧客户端已移除、新客户端未加入）：
// 租约到期后恢复必须只补加期望凭证，不得重建端口，也不得留下空入站。
func TestRotationRecoversFromACrashBetweenRemoveAndAdd(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tag, port := f.tagOf(record), record.Inbound.Inbound.Port
	f.rotate(t, record)

	// 模拟崩溃：AddUser 之前进程消失，入站里已经没有受管客户端。
	f.adapter.OnAddUser = func() {
		delete(f.adapter.Users[tag], record.Identity.StatisticsID)
		panic("crash between remove and add")
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = f.sync.Drain(context.Background())
	}()
	if _, present := f.adapter.Users[tag][record.Identity.StatisticsID]; present {
		t.Fatal("the crash fixture did not leave the inbound empty")
	}

	// 租约到期后由同一操作恢复。
	f.clock.Time = f.clock.Time.Add(11 * time.Second)
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after crash recovery")
	if !f.adapter.Listening(port) {
		t.Fatalf("port %d stopped listening during recovery", port)
	}
	after, _ := f.store.User(context.Background(), record.User.ID)
	if after.Allocation.PendingSync() || after.Inbound.Inbound.Port != port || after.Inbound.Inbound.InboundTag != tag {
		t.Fatalf("recovery moved or left the allocation pending: %#v", after.Allocation)
	}
	remote := f.adapter.Users[tag][record.Identity.StatisticsID]
	if remote.CredentialVersion != after.Allocation.DesiredCredentialVersion {
		t.Fatalf("recovered credential version = %d want %d", remote.CredentialVersion, after.Allocation.DesiredCredentialVersion)
	}
}

// 协调器重放：入站在但受管客户端缺失时，重建意图不得因为「标签已存在」而被误判为已收敛。
func TestReconcileDoesNotConfirmAnInboundWithoutItsManagedClient(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tag, port := f.tagOf(record), record.Inbound.Inbound.Port
	// 制造「入站在、客户端缺失」的坏状态，并让一次新的创建意图去处理它。
	delete(f.adapter.Users[tag], record.Identity.StatisticsID)
	if _, err := f.users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: false,
		ExpectedRevision: record.User.Revision, RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := f.store.User(context.Background(), record.User.ID)
	if _, err := f.users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: true,
		ExpectedRevision: current.User.Revision, RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after re-enable")
	if _, ok := f.adapter.Users[tag][record.Identity.StatisticsID]; !ok {
		t.Fatal("the rebuilt inbound does not carry the managed client")
	}
	if !f.adapter.Listening(port) {
		t.Fatalf("port %d is not listening after the rebuild", port)
	}
}
