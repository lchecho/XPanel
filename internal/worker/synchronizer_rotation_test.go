package worker

import (
	"context"
	"fmt"
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

// 加回新凭证失败时：入站里必须仍有受管客户端（过渡客户端守着），端口不中断；
// 故障解除后轮换补完，最终只剩期望身份（FR-017/FR-019）。
func TestRotationKeepsTheInboundAliveWhenTheNewCredentialCannotBeAdded(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tag, port := f.tagOf(record), record.Inbound.Inbound.Port
	safety := domain.RotationSafetyID(record.Identity.StatisticsID)
	f.rotate(t, record)

	// 第一次 add_user 是过渡客户端（放行并生效），第二次是加回期望凭证（失败）：
	// 此时入站里只剩过渡客户端，它必须守住这条入站。
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Applied: true},
		{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected, Operation: "add_user", Retryable: false, SafeSummary: "rejected"}}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after a failed rotation")
	if _, present := f.adapter.Inbounds[tag]; !present {
		t.Fatal("the inbound was torn down instead of being held by the transition client")
	}
	if !f.adapter.Listening(port) {
		t.Fatalf("port %d stopped listening during a failed rotation", port)
	}
	if _, held := f.adapter.Users[tag][safety]; !held {
		t.Fatalf("the transition client is not holding the inbound: %#v", f.adapter.Users[tag])
	}

	// 故障解除后重试补完轮换，过渡客户端被清理，只剩期望身份。
	delete(f.adapter.Failures, "add_user")
	_, _, next := f.operationState(t, record.Allocation.ID)
	if next.After(f.clock.Now()) {
		f.clock.Time = next
	}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertNoEmptyInbound(t, "after the retry")
	after, _ := f.store.User(context.Background(), record.User.ID)
	if after.Allocation.PendingSync() || after.Inbound.Inbound.Port != port {
		t.Fatalf("allocation after the retry = %#v", after.Allocation)
	}
	if got := f.adapter.Users[tag]; len(got) != 1 {
		t.Fatalf("inbound holds %d clients after rotation, want exactly the managed one: %#v", len(got), got)
	}
	remote := f.adapter.Users[tag][record.Identity.StatisticsID]
	if remote.CredentialVersion != after.Allocation.DesiredCredentialVersion {
		t.Fatalf("credential version = %d want %d", remote.CredentialVersion, after.Allocation.DesiredCredentialVersion)
	}
	if !f.adapter.Listening(port) {
		t.Fatalf("port %d is not listening after the rotation completed", port)
	}
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

// T084：在轮换的每一个 RPC 边界注入进程崩溃，断言入站的受管客户端数从不为 0、端口持续监听，
// 恢复后最终只剩原统计身份对应的新凭证，且流量历史归属不变。
func TestRotationSurvivesACrashAtEveryRPCBoundary(t *testing.T) {
	// 一次成功轮换的 RPC 序列：list_users(读) → add_user(过渡) → remove_user(旧) → add_user(新) → remove_user(过渡) → ConfirmSync。
	// boundary 0–3 是四次变更 RPC 之前崩溃；boundary 4 是四次 RPC 全部成功、但确认事务之前崩溃。
	for boundary := 0; boundary < 5; boundary++ {
		t.Run(fmt.Sprintf("crash_before_rpc_%d", boundary), func(t *testing.T) {
			f := newSyncFixture(t)
			record := f.createUser(t, "Alice")
			if _, err := f.sync.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			tag, port := f.tagOf(record), record.Inbound.Inbound.Port
			// 轮换前先积累流量，确认历史归属在轮换后仍然连续。
			f.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 4096)
			f.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 8192)
			f.rotate(t, record)

			// 在第 boundary 次变更类 RPC 之前崩溃；每次调用前都检查不变量。
			// 钩子被 fake 取用后即清空，必须每次重新挂上，否则只有第一次调用会被拦截。
			calls, crashed := 0, false
			var crash func()
			crash = func() {
				f.assertNoEmptyInbound(t, fmt.Sprintf("before rpc %d", calls))
				if !f.adapter.Listening(port) {
					t.Fatalf("port %d stopped listening before rpc %d", port, calls)
				}
				if calls == boundary {
					crashed = true
					panic("crash")
				}
				calls++
				f.adapter.OnAddUser, f.adapter.OnRemoveUser = crash, crash
			}
			f.adapter.OnAddUser, f.adapter.OnRemoveUser = crash, crash
			if boundary == 4 {
				// 四次 RPC 都成功之后、ConfirmSync 之前崩溃：此时 Xray 已是期望状态，
				// 但库里还没记下来，恢复必须重放整套过渡且不破坏已经正确的状态。
				f.sync.store = &crashOnConfirmStore{Store: f.sync.store}
			}
			func() {
				defer func() { _ = recover() }()
				_, _ = f.sync.Drain(context.Background())
			}()
			if boundary == 4 {
				f.sync.store = f.store
			}
			// boundary 0–3 对应四次变更 RPC，必须真的崩在那一步；boundary 4 崩在确认事务之前。
			if boundary < 4 && !crashed {
				t.Fatalf("boundary %d was never reached: only %d mutation RPCs happened", boundary, calls)
			}
			if boundary == 4 && calls != 4 {
				t.Fatalf("boundary 4 expects all four mutation RPCs to run, got %d", calls)
			}
			f.assertNoEmptyInbound(t, "right after the crash")
			if !f.adapter.Listening(port) {
				t.Fatalf("port %d stopped listening after the crash", port)
			}

			// 租约到期后恢复：同一操作续上，最终状态是「恰好一个受管客户端且是期望身份」。
			f.adapter.OnAddUser, f.adapter.OnRemoveUser = nil, nil
			f.clock.Time = f.clock.Time.Add(11 * time.Second)
			if _, err := f.sync.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.assertNoEmptyInbound(t, "after recovery")
			after, _ := f.store.User(context.Background(), record.User.ID)
			if after.Allocation.PendingSync() {
				t.Fatalf("allocation still pending after recovery: %#v", after.Allocation)
			}
			if got := f.adapter.Users[tag]; len(got) != 1 {
				t.Fatalf("inbound holds %d clients, want exactly one: %#v", len(got), got)
			}
			remote, ok := f.adapter.Users[tag][record.Identity.StatisticsID]
			if !ok || remote.CredentialVersion != after.Allocation.DesiredCredentialVersion {
				t.Fatalf("final client = %#v, want the managed identity at version %d", remote,
					after.Allocation.DesiredCredentialVersion)
			}
			if after.Identity.StatisticsID != record.Identity.StatisticsID {
				t.Fatal("rotation changed the immutable statistics identity")
			}
			if !f.adapter.Listening(port) || after.Inbound.Inbound.Port != port {
				t.Fatalf("port moved or stopped: %d", after.Inbound.Inbound.Port)
			}
			// 过渡身份不得残留，也不得进入连接信息。
			if _, leftover := f.adapter.Users[tag][domain.RotationSafetyID(record.Identity.StatisticsID)]; leftover {
				t.Fatal("the transition client survived the rotation")
			}
			// 流量计数按原统计身份继续累计：历史归属不变。
			if f.adapter.Counters[record.Identity.StatisticsID][ports.Uplink] != 4096 {
				t.Fatalf("traffic history was lost: %#v", f.adapter.Counters[record.Identity.StatisticsID])
			}
		})
	}
}

// 过渡身份是面板内部状态：它不得出现在连接信息，也不得进入流量采集目标。
func TestRotationTransitionClientNeverLeaksIntoUserFacingState(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tag := f.tagOf(record)
	safety := domain.RotationSafetyID(record.Identity.StatisticsID)
	// 让轮换停在「过渡客户端已就位」的中间态。
	f.rotate(t, record)
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Applied: true},
		{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected, Operation: "add_user", Retryable: false, SafeSummary: "rejected"}}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, held := f.adapter.Users[tag][safety]; !held {
		t.Fatal("the transition client is not present in the intermediate state")
	}
	// 采集目标里只有真实身份。
	targets, err := f.store.CollectionTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if domain.IsRotationSafetyID(target.Identity.StatisticsID) {
			t.Fatalf("the transition identity entered traffic collection: %#v", target)
		}
	}
	// 库里没有为过渡身份登记任何身份行。
	var registered int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM xray_user_identities WHERE statistics_id=?`, safety).Scan(&registered)
	if registered != 0 {
		t.Fatalf("the transition identity was persisted %d times", registered)
	}
}

// crashOnConfirmStore 在第一次 ConfirmSync 时模拟进程崩溃：Xray 已经是期望状态，库里还没记下来。
type crashOnConfirmStore struct {
	ports.Store
	crashed bool
}

func (s *crashOnConfirmStore) ConfirmSync(ctx context.Context, operationID domain.ID, owner string,
	revision domain.Revision, credentialVersion int64, present bool, now time.Time) (bool, error) {
	if !s.crashed {
		s.crashed = true
		panic("crash before the confirmation transaction")
	}
	return s.Store.ConfirmSync(ctx, operationID, owner, revision, credentialVersion, present, now)
}
