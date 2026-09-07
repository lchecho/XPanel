package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// T128：worker 处理旧操作期间管理员提交了新意图；旧操作的失败结果只能结束旧操作，不得把 allocation 改回 pending/error。
func TestLateFailureOfSupersededOperationDoesNotOverwriteNewerIntent(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Racy")
	f.adapter.Delay = 300 * time.Millisecond
	f.adapter.Failures["create_inbound"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected, Operation: "add_user", SafeSummary: "rejected"}}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = f.sync.Drain(ctx) // 领取 create（rev1），在慢 RPC 中等待
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := f.users.SetAdminEnabled(ctx, application.SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	f.adapter.Delay = 0
	after, _ := f.store.User(ctx, record.User.ID)
	// 旧操作的失败结果不得写入 allocation（无错误码、不为 error）；同一轮 Drain 可能已继续处理新意图。
	if after.Allocation.DesiredRevision != 2 || after.Allocation.ProjectionState == domain.ProjectionError || after.Allocation.LastSyncErrorCode != "" {
		t.Fatalf("late failure overwrote newer intent: %#v", after.Allocation)
	}
	var oldState string
	_ = f.store.DB().Read.QueryRow(`SELECT state FROM synchronization_operations WHERE allocation_id=? AND desired_revision=1`, record.Allocation.ID.String()).Scan(&oldState)
	if oldState != string(domain.SyncSuperseded) {
		t.Fatalf("old operation state = %s", oldState)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ = f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionAbsent || after.Allocation.SyncedRevision != 2 || after.Allocation.PendingSync() {
		t.Fatalf("newer intent did not converge: %#v", after.Allocation)
	}
}

// T128：两个 worker 实例并发领取时只有一个成功，落后者的结果被忽略。
func TestTwoWorkersOnOneDatabaseDoNotDoubleApply(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Shared")
	second := NewSynchronizer(f.store, f.adapter, f.keyring, f.clock, nil, f.sync.node, SynchronizerOptions{Owner: "second",
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, RPCTimeout: time.Second, Random: func(n int64) int64 { return n - 1 }})
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i, worker := range []*Synchronizer{f.sync, second} {
		wg.Add(1)
		go func(i int, worker *Synchronizer) {
			defer wg.Done()
			processed, _ := worker.Drain(ctx)
			results[i] = processed
		}(i, worker)
	}
	wg.Wait()
	if results[0]+results[1] != 1 || f.calls("create_inbound") != 1 {
		t.Fatalf("processed=%v add_user calls=%d", results, f.calls("create_inbound"))
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPresent || after.Allocation.SyncedRevision != 1 {
		t.Fatalf("allocation = %#v", after.Allocation)
	}
}
