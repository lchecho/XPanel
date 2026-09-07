package application

import (
	"context"
	"sync"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func confirmLatest(t *testing.T, fixture *featureFixture, allocationID domain.ID, present bool) {
	t.Helper()
	var opID string
	var revision int64
	var version int64
	if err := fixture.store.DB().Read.QueryRow(`SELECT id,desired_revision,COALESCE(desired_credential_version,1) FROM synchronization_operations
        WHERE allocation_id=? AND state IN ('pending','retry_wait','leased') ORDER BY desired_revision DESC LIMIT 1`, allocationID.String()).Scan(&opID, &revision, &version); err != nil {
		t.Fatal(err)
	}
	leaseForTest(t, fixture, domain.ID(opID))
	if ok, err := fixture.store.ConfirmSync(context.Background(), domain.ID(opID), "test", domain.Revision(revision), version, present, fixture.clock.Now()); err != nil || !ok {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
}

func newReconciliation(fixture *featureFixture, notify func()) *ReconciliationService {
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	return NewReconciliationService(fixture.store, fixture.adapter, fixture.clock, target, &sync.Mutex{}, 15*time.Second, notify, nil, nil)
}

func TestReconcileRestoresOnlyDesiredUsersAndRemovesUnknownNamespaceIdentities(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(100)
	active := presentUser(t, fixture, "Active", nil)
	disabled := presentUser(t, fixture, "Disabled", nil)
	exceeded := presentUser(t, fixture, "Exceeded", &limit)
	deleted := presentUser(t, fixture, "Deleted", nil)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	if _, err := users.SetAdminEnabled(context.Background(), SetEnabledInput{ID: disabled.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	confirmLatest(t, fixture, disabled.Allocation.ID, false)
	absentInbound(fixture, disabled)
	fixture.adapter.SetCounter(exceeded.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(exceeded.Identity.StatisticsID, ports.Downlink, 0)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	confirmLatest(t, fixture, exceeded.Allocation.ID, false)
	absentInbound(fixture, exceeded)
	if _, err := users.DeleteUser(context.Background(), LifecycleInput{ID: deleted.User.ID, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	confirmLatest(t, fixture, deleted.Allocation.ID, false)
	absentInbound(fixture, deleted)
	fixture.adapter.AddExternalInbound("operator-inbound", 45000)

	woken := 0
	service := newReconciliation(fixture, func() { woken++ })
	summary, err := service.ReconcileOnce(context.Background())
	if err != nil || summary.Drift != 0 || summary.RemovedUnknown != 0 || woken != 0 {
		t.Fatalf("steady-state summary = %#v, %v woken=%d", summary, err, woken)
	}

	// 重启清空全部面板入站：只有 Active 需要重建；命名空间内的孤立入站与运维自有入站分别处理。
	fixture.adapter.Restart()
	fixture.adapter.AddExternalInbound("operator-inbound", 45000)
	fixture.adapter.AddOrphanInbound("xpanel-00000000-0000-4000-8000-00000000dead", 45001)
	summary, err = service.ReconcileOnce(context.Background())
	if err != nil || summary.Drift != 1 || summary.RemovedUnknown != 1 || woken != 1 {
		t.Fatalf("restart summary = %#v, %v woken=%d", summary, err, woken)
	}
	if operationsFor(t, fixture, active.Allocation.ID, "reconcile") != 1 {
		t.Fatal("reconcile operation for the active user missing")
	}
	for _, record := range []ports.UserRecord{disabled, exceeded, deleted} {
		if operationsFor(t, fixture, record.Allocation.ID, "reconcile") != 0 {
			t.Fatalf("%s received a reconcile operation", record.User.DisplayName)
		}
	}
	if _, stillThere := fixture.adapter.Inbounds["xpanel-00000000-0000-4000-8000-00000000dead"]; !stillThere {
		t.Fatal("reconciler removed the orphan inbound synchronously instead of persisting an intent")
	}
	var queued int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE state='pending'`).Scan(&queued)
	if queued != 1 {
		t.Fatalf("queued drift removals = %d", queued)
	}
	if _, kept := fixture.adapter.Inbounds["operator-inbound"]; !kept {
		t.Fatal("operator inbound outside the panel namespace was removed")
	}
	// 第二轮：已有待处理操作与移除意图，不重复入队。
	summary, err = service.ReconcileOnce(context.Background())
	if err != nil || summary.Drift != 0 || summary.RemovedUnknown != 0 || operationsFor(t, fixture, active.Allocation.ID, "reconcile") != 1 {
		t.Fatalf("second round summary = %#v, %v", summary, err)
	}
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals`).Scan(&queued)
	if queued != 1 {
		t.Fatalf("drift removals after replay = %d", queued)
	}
	// 持续不同步：超过 3×interval 仍未确认。
	fixture.clock.Advance(46 * time.Second)
	summary, _ = service.ReconcileOnce(context.Background())
	if summary.StuckSync != 1 {
		t.Fatalf("stuck sync = %d", summary.StuckSync)
	}
}

func TestReconcileTracksUnavailabilityAndReconnect(t *testing.T) {
	fixture := newFeatureFixture(t)
	presentUser(t, fixture, "Solo", nil)
	revalidated := 0
	service := NewReconciliationService(fixture.store, fixture.adapter, fixture.clock, ports.InstanceTarget{}, &sync.Mutex{}, 15*time.Second, nil,
		func(context.Context, domain.ID) error { revalidated++; return nil }, nil)
	fixture.adapter.Available = false
	if _, err := service.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("reconcile succeeded while Xray is unavailable")
	}
	instance, _ := fixture.store.ManagedInstance(context.Background())
	if instance.HealthState != "unreachable" {
		t.Fatalf("instance health = %s", instance.HealthState)
	}
	profiles, _ := fixture.store.Templates(context.Background(), false)
	if err := fixture.store.SetTemplateCompatibility(context.Background(), profiles[0].Template.ID, domain.CompatibilityUnreachable, "down", fixture.clock.Now()); err != nil {
		t.Fatal(err)
	}
	fixture.adapter.Available = true
	summary, err := service.ReconcileOnce(context.Background())
	if err != nil || !summary.Reconnected || revalidated != 1 {
		t.Fatalf("reconnect summary = %#v, %v revalidated=%d", summary, err, revalidated)
	}
	instance, _ = fixture.store.ManagedInstance(context.Background())
	if instance.HealthState != "healthy" || instance.BootEpoch == "" {
		t.Fatalf("instance after reconnect = %#v", instance)
	}
}
