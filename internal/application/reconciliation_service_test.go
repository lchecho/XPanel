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
	if ok, err := fixture.store.ConfirmSync(context.Background(), domain.ID(opID), domain.Revision(revision), version, present, fixture.clock.Now()); err != nil || !ok {
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
	delete(fixture.adapter.Users["managed"], disabled.Identity.StatisticsID)
	fixture.adapter.SetCounter(exceeded.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(exceeded.Identity.StatisticsID, ports.Downlink, 0)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	confirmLatest(t, fixture, exceeded.Allocation.ID, false)
	delete(fixture.adapter.Users["managed"], exceeded.Identity.StatisticsID)
	if _, err := users.DeleteUser(context.Background(), LifecycleInput{ID: deleted.User.ID, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	confirmLatest(t, fixture, deleted.Allocation.ID, false)
	delete(fixture.adapter.Users["managed"], deleted.Identity.StatisticsID)
	fixture.adapter.Users["managed"]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
	fixture.adapter.Users["managed"]["operator-static"] = ports.RemoteUser{StatisticsID: "operator-static", Present: true, Kind: "external"}

	woken := 0
	service := newReconciliation(fixture, func() { woken++ })
	summary, err := service.ReconcileOnce(context.Background())
	if err != nil || summary.Drift != 0 || summary.RemovedUnknown != 0 || woken != 0 {
		t.Fatalf("steady-state summary = %#v, %v woken=%d", summary, err, woken)
	}

	// 重启丢失动态用户：只有 Active 需要恢复。
	fixture.adapter.Restart()
	fixture.adapter.Users["managed"]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
	fixture.adapter.Users["managed"]["operator-static"] = ports.RemoteUser{StatisticsID: "operator-static", Present: true, Kind: "external"}
	fixture.adapter.Users["managed"]["xpanel-00000000-0000-4000-8000-00000000dead"] = ports.RemoteUser{StatisticsID: "xpanel-00000000-0000-4000-8000-00000000dead", Present: true, Kind: "managed"}
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
	if _, stillThere := fixture.adapter.Users["managed"]["xpanel-00000000-0000-4000-8000-00000000dead"]; stillThere {
		t.Fatal("unknown namespace identity was not removed")
	}
	if _, kept := fixture.adapter.Users["managed"]["operator-static"]; !kept {
		t.Fatal("external identity was removed")
	}
	if _, kept := fixture.adapter.Users["managed"]["bootstrap"]; !kept {
		t.Fatal("bootstrap identity was removed")
	}
	var audits int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, domain.ActionReconcileRemovedUnknown).Scan(&audits)
	if audits != 1 {
		t.Fatalf("unknown-removal audits = %d", audits)
	}
	// 第二轮：已有待处理操作，不重复入队。
	summary, err = service.ReconcileOnce(context.Background())
	if err != nil || summary.Drift != 0 || operationsFor(t, fixture, active.Allocation.ID, "reconcile") != 1 {
		t.Fatalf("second round summary = %#v, %v", summary, err)
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
	profiles, _ := fixture.store.Profiles(context.Background(), false)
	if err := fixture.store.SetProfileCompatibility(context.Background(), profiles[0].Profile.ID, domain.CompatibilityUnreachable, "down", fixture.clock.Now()); err != nil {
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
