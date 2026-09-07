package worker

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func TestReconcilerRestoresActiveUsersAfterXrayRestart(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	active := f.createUser(t, "Active")
	disabled := f.createUser(t, "Disabled")
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users.SetAdminEnabled(ctx, application.SetEnabledInput{ID: disabled.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	f.adapter.Restart()
	// 重启清空全部面板入站；运维自有入站由运维配置恢复，面板不得触碰。
	f.adapter.AddExternalInbound("operator-inbound", 45000)
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	service := application.NewReconciliationService(f.store, f.adapter, f.clock, target, f.sync.node, 15*time.Second, f.sync.Wake, nil, nil)
	reconciler := NewReconciler(service, 15*time.Second, nil)
	reconciler.RunOnce(ctx)
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if present := f.userPresent(active); !present {
		t.Fatal("active user was not restored")
	}
	if present := f.userPresent(disabled); present {
		t.Fatal("disabled user was restored")
	}
	if _, present := f.adapter.Inbounds["operator-inbound"]; !present {
		t.Fatal("operator inbound outside the panel namespace was touched")
	}
	after, _ := f.store.User(ctx, active.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPresent || after.Allocation.PendingSync() {
		t.Fatalf("active allocation after reconcile = %#v", after.Allocation)
	}
	// Run 循环：Trigger 立即触发并在取消时退出。
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { reconciler.Run(runCtx); close(done) }()
	reconciler.Trigger()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler did not stop")
	}
}
