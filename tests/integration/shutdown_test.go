package integration

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// 优雅关闭：在 Xray 调用进行中取消 worker，不得留下部分提交；重启后从同一操作恢复到相同结果。
func TestWorkerCancellationLeavesNoPartialCommit(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Adapter.Delay = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Sync.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond) // worker 已领取操作并处于慢 RPC 中
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("synchronizer did not stop while an RPC was in flight")
	}
	after := app.User(record.User.ID)
	if after.Allocation.ProjectionState == domain.ProjectionPresent || after.Credential.State != domain.CredentialPending || after.Allocation.SyncedRevision != 0 {
		t.Fatalf("partial state committed during shutdown: %#v credential=%s", after.Allocation, after.Credential.State)
	}
	if _, present := app.Adapter.Users["managed"][record.Identity.StatisticsID]; present {
		t.Fatal("cancelled RPC applied the change in fake Xray")
	}
	var state string
	_ = app.Store.DB().Read.QueryRow(`SELECT state FROM synchronization_operations WHERE allocation_id=?`, record.Allocation.ID.String()).Scan(&state)
	if state != string(domain.SyncLeased) && state != string(domain.SyncRetryWait) && state != string(domain.SyncPending) {
		t.Fatalf("operation state after cancel = %s", state)
	}
	// 重启：租约到期/退避结束后同一操作继续执行并收敛。
	app.Adapter.Delay = 0
	app.Clock.Advance(time.Minute)
	if processed := app.Drain(); processed != 1 {
		t.Fatalf("resumed operations = %d", processed)
	}
	after = app.User(record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPresent || after.Allocation.PendingSync() {
		t.Fatalf("allocation after resume = %#v", after.Allocation)
	}
	var operations int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=?`, record.Allocation.ID.String()).Scan(&operations)
	if operations != 1 {
		t.Fatalf("operations duplicated during recovery: %d", operations)
	}
}
