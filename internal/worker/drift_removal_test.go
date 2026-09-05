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

const unknownIdentity = "xpanel-00000000-0000-4000-8000-00000000dead"

func (f *syncFixture) reconciliation() *application.ReconciliationService {
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	return application.NewReconciliationService(f.store, f.adapter, f.clock, target, f.sync.node, 15*time.Second, f.sync.Wake, nil, nil)
}

func (f *syncFixture) driftState(t *testing.T) (string, int) {
	t.Helper()
	var state string
	var count int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals`).Scan(&count)
	_ = f.store.DB().Read.QueryRow(`SELECT state FROM drift_removals ORDER BY created_at DESC LIMIT 1`).Scan(&state)
	return state, count
}

// T130：未知身份的移除是持久化意图：RPC 超时后重试、崩溃后从租约恢复、协调重放不产生重复，成功后写审计。
func TestDriftRemovalIsPersistentRetryableAndAudited(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	f.createUser(t, "Known")
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	f.adapter.Users[f.tag][unknownIdentity] = ports.RemoteUser{StatisticsID: unknownIdentity, Present: true, Kind: "managed"}
	f.adapter.Users[f.tag]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
	reconcile := f.reconciliation()
	summary, err := reconcile.ReconcileOnce(ctx)
	if err != nil || summary.RemovedUnknown != 1 {
		t.Fatalf("summary = %#v, %v", summary, err)
	}
	if _, present := f.adapter.Users[f.tag][unknownIdentity]; !present {
		t.Fatal("identity removed before the intent was executed by the worker")
	}
	// 重放协调：不重复入队。
	if summary, _ = reconcile.ReconcileOnce(ctx); summary.RemovedUnknown != 0 {
		t.Fatalf("replayed reconcile queued again: %#v", summary)
	}
	// RPC 超时且未生效 → 重试等待。
	f.adapter.Failures["remove_user"] = []xrayfake.Failure{{Err: deadline("remove_user")}}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	state, count := f.driftState(t)
	if state != string(domain.SyncRetryWait) || count != 1 {
		t.Fatalf("after timeout state=%s count=%d", state, count)
	}
	if _, present := f.adapter.Users[f.tag][unknownIdentity]; !present {
		t.Fatal("identity vanished although the RPC failed")
	}
	// 崩溃模拟：另一实例领取后失联，租约过期由本实例恢复。
	var next int64
	_ = f.store.DB().Read.QueryRow(`SELECT next_attempt_at FROM drift_removals`).Scan(&next)
	f.clock.Set(time.UnixMilli(next).UTC())
	if removal, err := f.store.LeaseDueDriftRemoval(ctx, "crashed", f.clock.Now(), time.Second); err != nil || removal == nil {
		t.Fatalf("lease = %#v, %v", removal, err)
	}
	if processed, _ := f.sync.Drain(ctx); processed != 0 {
		t.Fatal("live lease stolen")
	}
	f.clock.Advance(2 * time.Second)
	if processed, err := f.sync.Drain(ctx); err != nil || processed != 1 {
		t.Fatalf("resume processed=%d, %v", processed, err)
	}
	if _, present := f.adapter.Users[f.tag][unknownIdentity]; present {
		t.Fatal("unknown identity still present after removal")
	}
	if _, kept := f.adapter.Users[f.tag]["bootstrap"]; !kept {
		t.Fatal("bootstrap identity was removed")
	}
	state, count = f.driftState(t)
	if state != string(domain.SyncSucceeded) || count != 1 {
		t.Fatalf("final state=%s count=%d", state, count)
	}
	var audits int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result='succeeded'`, domain.ActionReconcileRemovedUnknown).Scan(&audits)
	if audits != 1 {
		t.Fatalf("removal audits = %d", audits)
	}
	// 收敛后再协调不再入队；已生效但超时的移除通过读后写判定完成。
	if summary, _ = reconcile.ReconcileOnce(ctx); summary.RemovedUnknown != 0 {
		t.Fatalf("post-convergence reconcile queued again: %#v", summary)
	}
	f.adapter.Users[f.tag][unknownIdentity] = ports.RemoteUser{StatisticsID: unknownIdentity, Present: true, Kind: "managed"}
	f.adapter.Failures["remove_user"] = []xrayfake.Failure{{Err: deadline("remove_user"), Applied: true}}
	if _, err := reconcile.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if state, count = f.driftState(t); state != string(domain.SyncSucceeded) || count != 2 {
		t.Fatalf("uncertain removal state=%s count=%d", state, count)
	}
}
