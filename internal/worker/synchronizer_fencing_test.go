package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func secondWorker(f *syncFixture) *Synchronizer {
	// 第二个进程有自己的 node 锁；只共享数据库。
	return NewSynchronizer(f.store, f.adapter, f.keyring, f.clock, nil, &sync.Mutex{}, SynchronizerOptions{Owner: "second",
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, Random: func(n int64) int64 { return n - 1 }})
}

func countAudits(t *testing.T, f *syncFixture, action string, operationID string) int {
	t.Helper()
	var n int
	if err := f.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND operation_id=?`, action, operationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// T143：慢 RPC 超过租期，第二个 worker 回收并完成轮换，同时管理员提交新意图；
// 旧 worker 在 RPC 返回后不得再调用 Xray（不进入 add 阶段）、不得确认旧操作，也不得把分配短暂改回旧意图。
func TestLostLeaseWorkerNeitherCallsNorConfirmsAfterReclaim(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Fenced")
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	second := secondWorker(f)
	if _, err := f.users.RotateCredential(ctx, application.LifecycleInput{ID: record.User.ID, ExpectedRevision: 0, RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	var rotateOp string
	_ = f.store.DB().Read.QueryRow(`SELECT id FROM synchronization_operations WHERE allocation_id=? AND desired_revision=2`, record.Allocation.ID.String()).Scan(&rotateOp)
	var reclaimed int
	f.adapter.OnRemoveUser = func() {
		// worker A 的 remove RPC 在途：租约到期，worker B 回收并完成整个轮换，然后管理员提交禁用意图。
		f.clock.Advance(11 * time.Second)
		processed, err := second.Drain(ctx)
		if err != nil {
			t.Errorf("second worker drain: %v", err)
		}
		reclaimed = processed
		current, _ := f.store.User(ctx, record.User.ID)
		if current.Allocation.SyncedRevision != 2 || current.Allocation.PendingSync() {
			t.Errorf("second worker did not complete the rotation: %#v", current.Allocation)
		}
		if _, err := f.users.SetAdminEnabled(ctx, application.SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: current.User.Revision,
			RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
			t.Errorf("disable: %v", err)
		}
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if reclaimed != 1 {
		t.Fatalf("second worker processed %d operations, want 1", reclaimed)
	}
	// 只有 B 添加过新凭证：创建 1 次 + B 的轮换 1 次；A 丢失租约后没有进入 add 阶段。
	if f.calls("add_user") != 2 {
		t.Fatalf("add_user calls = %d, want 2", f.calls("add_user"))
	}
	if n := countAudits(t, f, domain.ActionSyncSucceeded, rotateOp); n != 1 {
		t.Fatalf("rotation confirmed %d times, want exactly once (by the reclaiming worker)", n)
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionAbsent || after.Allocation.SyncedRevision != 3 || after.Allocation.PendingSync() {
		t.Fatalf("newest intent not converged: %#v", after.Allocation)
	}
	if _, present := f.adapter.Users[f.tag][record.Identity.StatisticsID]; present {
		t.Fatal("user present in Xray although the newest intent is disabled")
	}
	var open int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND state NOT IN ('succeeded','superseded')`, record.Allocation.ID.String()).Scan(&open)
	if open != 0 {
		t.Fatalf("open operations = %d", open)
	}
}

// T143：过期租约只能从旧 owner 手中回收；仍在租期内的租约不能被抢占，续租失败的 worker 不得确认。
func TestConfirmRequiresLeaseOwnership(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Owned")
	work, err := f.store.LeaseDueSync(ctx, "alpha", f.clock.Now(), 10*time.Second)
	if err != nil || work == nil {
		t.Fatalf("lease = %#v, %v", work, err)
	}
	if stolen, _ := f.store.LeaseDueSync(ctx, "beta", f.clock.Now().Add(5*time.Second), 10*time.Second); stolen != nil {
		t.Fatal("live lease was stolen")
	}
	f.clock.Advance(11 * time.Second)
	reclaimed, err := f.store.LeaseDueSync(ctx, "beta", f.clock.Now(), 10*time.Second)
	if err != nil || reclaimed == nil || reclaimed.Operation.ID != work.Operation.ID {
		t.Fatalf("expired lease not reclaimed: %#v, %v", reclaimed, err)
	}
	if held, _ := f.store.RenewSyncLease(ctx, work.Operation.ID, "alpha", f.clock.Now(), 10*time.Second); held {
		t.Fatal("old owner renewed a reclaimed lease")
	}
	if ok, err := f.store.ConfirmSync(ctx, work.Operation.ID, "alpha", 1, 1, true, f.clock.Now()); err != nil || ok {
		t.Fatalf("old owner confirmed: ok=%v err=%v", ok, err)
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPending || after.Allocation.SyncedRevision != 0 {
		t.Fatalf("stale confirm changed the allocation: %#v", after.Allocation)
	}
	if ok, err := f.store.ConfirmSync(ctx, work.Operation.ID, "beta", 1, 1, true, f.clock.Now()); err != nil || !ok {
		t.Fatalf("current owner confirm: ok=%v err=%v", ok, err)
	}
}

// crashingStore 模拟“外部移除 RPC 已成功但完成写入前崩溃”：CompleteDriftRemoval 第一次调用返回错误。
type crashingStore struct {
	ports.Store
	crashed bool
}

func (c *crashingStore) CompleteDriftRemoval(ctx context.Context, id domain.ID, owner string, now time.Time, audit domain.AuditEvent) error {
	if !c.crashed {
		c.crashed = true
		return errCrash
	}
	return c.Store.CompleteDriftRemoval(ctx, id, owner, now, audit)
}

var errCrash = errors.New("simulated crash before completion")

// T143/T151：外部移除已生效但完成写入前崩溃，租约到期后另一 worker 回收重放：
// 回收者先读原入站的实际身份，已不存在则直接完成——外部移除、完成状态与审计各恰好一次。
func TestReclaimedDriftRemovalIsNotExecutedTwice(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	const unknown = "xpanel-ffffffff-1111-4111-8111-ffffffffffff"
	if f.adapter.Users[f.tag] == nil {
		f.adapter.Users[f.tag] = map[string]ports.RemoteUser{}
	}
	f.adapter.Users[f.tag][unknown] = ports.RemoteUser{StatisticsID: unknown, Present: true, Kind: "managed"}
	if _, err := f.store.EnqueueDriftRemoval(ctx, f.profile, unknown, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	crashing := NewSynchronizer(&crashingStore{Store: f.store}, f.adapter, f.keyring, f.clock, nil, &sync.Mutex{}, SynchronizerOptions{Owner: "crashing",
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, RPCTimeout: time.Second, Random: func(n int64) int64 { return n - 1 }})
	if _, err := crashing.Drain(ctx); !errors.Is(err, errCrash) {
		t.Fatalf("crash drain err = %v", err)
	}
	if _, present := f.adapter.Users[f.tag][unknown]; present {
		t.Fatal("external removal did not take effect before the crash")
	}
	if state, _ := f.driftState(t); state != string(domain.SyncLeased) {
		t.Fatalf("removal state after crash = %s, want leased (lease still held by the crashed worker)", state)
	}
	// 未到期前不可回收；到期后第二个 worker 回收并读后完成，不再调用 RemoveUser。
	if processed, _ := secondWorker(f).Drain(ctx); processed != 0 {
		t.Fatal("live lease reclaimed early")
	}
	f.clock.Advance(11 * time.Second)
	if processed, err := secondWorker(f).Drain(ctx); err != nil || processed != 1 {
		t.Fatalf("reclaim drain = %d, %v", processed, err)
	}
	if f.calls("remove_user") != 1 {
		t.Fatalf("remove_user calls = %d, want exactly 1 (reclaim must read before removing)", f.calls("remove_user"))
	}
	var removals, succeeded int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*),SUM(state='succeeded') FROM drift_removals`).Scan(&removals, &succeeded)
	if removals != 1 || succeeded != 1 {
		t.Fatalf("drift removals = %d succeeded = %d", removals, succeeded)
	}
	var audits int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, domain.ActionReconcileRemovedUnknown).Scan(&audits)
	if audits != 1 {
		t.Fatalf("drift removal audited %d times, want 1", audits)
	}
	// 再次 Drain 没有可领取的工作，也不会再调用 Xray。
	if processed, _ := f.sync.Drain(ctx); processed != 0 || f.calls("remove_user") != 1 {
		t.Fatalf("replay drain processed=%d remove_user=%d", processed, f.calls("remove_user"))
	}
}

// T151：所有构造路径都有明确 RPC 超时，租约至少覆盖 3 次 RPC。
func TestSynchronizerLeaseCoversRPCTimeout(t *testing.T) {
	f := newSyncFixture(t)
	implicit := NewSynchronizer(f.store, f.adapter, f.keyring, f.clock, nil, &sync.Mutex{}, SynchronizerOptions{LeaseDuration: time.Second})
	if implicit.lease < 3*defaultRPCTimeout {
		t.Fatalf("lease %s does not cover the default RPC timeout", implicit.lease)
	}
	explicit := NewSynchronizer(f.store, f.adapter, f.keyring, f.clock, nil, &sync.Mutex{}, SynchronizerOptions{LeaseDuration: time.Second, RPCTimeout: 2 * time.Second})
	if explicit.lease != 6*time.Second {
		t.Fatalf("lease = %s, want 6s (3 × RPC timeout)", explicit.lease)
	}
}
