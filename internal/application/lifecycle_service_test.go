package application

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func credentialStates(t *testing.T, fixture *featureFixture, allocationID domain.ID) map[int64]string {
	t.Helper()
	rows, err := fixture.store.DB().Read.Query(`SELECT version,state,key_ciphertext IS NULL FROM access_credentials WHERE allocation_id=? ORDER BY version`, allocationID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[int64]string{}
	for rows.Next() {
		var version int64
		var state string
		var nullKey bool
		if err := rows.Scan(&version, &state, &nullKey); err != nil {
			t.Fatal(err)
		}
		if state == string(domain.CredentialDestroyed) != nullKey {
			t.Fatalf("credential v%d state=%s but key null=%v", version, state, nullKey)
		}
		result[version] = state
	}
	return result
}

func TestRotateCredentialCreatesPendingVersionAndDestroysOldOnConfirm(t *testing.T) {
	fixture := newFeatureFixture(t)
	record := presentUser(t, fixture, "Rotate", nil)
	woken := 0
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, func() { woken++ })
	connections := NewConnectionService(fixture.store, fixture.keyring)
	before, err := connections.BuildConnectionInfo(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := LifecycleInput{ID: record.User.ID, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	if replay, err := users.RotateCredential(context.Background(), input); err != nil || replay || woken != 1 {
		t.Fatalf("rotate = %v, %v woken=%d", replay, err, woken)
	}
	if replay, err := users.RotateCredential(context.Background(), input); err != nil || !replay {
		t.Fatalf("rotate replay = %v, %v", replay, err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Credential.Version != 2 || after.Credential.State != domain.CredentialPending || after.Allocation.DesiredCredentialVersion != 2 ||
		after.Allocation.DesiredRevision != 2 || after.User.Revision != 1 {
		t.Fatalf("after rotate = credential v%d %s allocation=%#v", after.Credential.Version, after.Credential.State, after.Allocation)
	}
	if _, err := connections.BuildConnectionInfo(context.Background(), record.User.ID); !errors.Is(err, ErrConnectionPending) {
		t.Fatalf("connection during rotation = %v", err)
	}
	var reason, phase string
	_ = fixture.store.DB().Read.QueryRow(`SELECT reason,phase FROM synchronization_operations WHERE allocation_id=? AND desired_revision=2`, record.Allocation.ID.String()).Scan(&reason, &phase)
	if reason != string(domain.SyncRotate) || phase != string(domain.SyncRemoveOld) {
		t.Fatalf("rotation operation = %s/%s", reason, phase)
	}
	again := LifecycleInput{ID: record.User.ID, ExpectedRevision: 1, RequestID: appID(t), ActorID: appID(t)}
	var conflict *domain.ConflictError
	if _, err := users.RotateCredential(context.Background(), again); !errors.As(err, &conflict) {
		t.Fatalf("second rotation error = %v", err)
	}
	var opID string
	_ = fixture.store.DB().Read.QueryRow(`SELECT id FROM synchronization_operations WHERE allocation_id=? AND desired_revision=2`, record.Allocation.ID.String()).Scan(&opID)
	if ok, err := fixture.store.ConfirmSync(context.Background(), domain.ID(opID), 2, 2, true, fixture.clock.Now()); err != nil || !ok {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
	states := credentialStates(t, fixture, record.Allocation.ID)
	if states[1] != string(domain.CredentialDestroyed) || states[2] != string(domain.CredentialActive) {
		t.Fatalf("credential states after confirm = %#v", states)
	}
	rotated, err := connections.BuildConnectionInfo(context.Background(), record.User.ID)
	if err != nil || rotated.Password.Reveal() == before.Password.Reveal() {
		t.Fatalf("rotated connection info = %v (same password=%v)", err, rotated.Password.Reveal() == before.Password.Reveal())
	}
}

func TestDeleteUserKeepsHistoryDestroysKeysAndAllowsNameReuse(t *testing.T) {
	fixture := newFeatureFixture(t)
	record := presentUser(t, fixture, "Deleted", nil)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 500)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 500)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	input := LifecycleInput{ID: record.User.ID, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	if replay, err := users.DeleteUser(context.Background(), input); err != nil || replay {
		t.Fatalf("delete = %v, %v", replay, err)
	}
	if replay, err := users.DeleteUser(context.Background(), input); err != nil || !replay {
		t.Fatalf("delete replay = %v, %v", replay, err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.User.Lifecycle != domain.LifecycleDeleted || after.Allocation.AdminEnabled || after.Allocation.DisplayState(after.User) != domain.DisplayDeleted {
		t.Fatalf("after delete = %#v", after.Allocation)
	}
	var stateErr *domain.InvalidStateError
	if _, err := users.RotateCredential(context.Background(), LifecycleInput{ID: record.User.ID, ExpectedRevision: 1, RequestID: appID(t), ActorID: appID(t)}); !errors.As(err, &stateErr) {
		t.Fatalf("rotate deleted user error = %v", err)
	}
	var opID string
	_ = fixture.store.DB().Read.QueryRow(`SELECT id FROM synchronization_operations WHERE allocation_id=? AND reason='delete'`, record.Allocation.ID.String()).Scan(&opID)
	if ok, err := fixture.store.ConfirmSync(context.Background(), domain.ID(opID), 2, 0, false, fixture.clock.Now()); err != nil || !ok {
		t.Fatalf("confirm removal = %v, %v", ok, err)
	}
	if states := credentialStates(t, fixture, record.Allocation.ID); states[1] != string(domain.CredentialDestroyed) {
		t.Fatalf("credentials after delete = %#v", states)
	}
	visible, _ := fixture.store.ListUsers(context.Background(), ports.UserFilter{})
	hidden, _ := fixture.store.ListUsers(context.Background(), ports.UserFilter{IncludeDeleted: true})
	if len(visible) != 0 || len(hidden) != 1 || hidden[0].Cycle.GrossUplinkBytes != 500 {
		t.Fatalf("lists visible=%d hidden=%d", len(visible), len(hidden))
	}
	reused := presentUser(t, fixture, "Deleted", nil)
	if reused.User.ID == record.User.ID || reused.Identity.StatisticsID == record.Identity.StatisticsID || reused.Cycle.GrossUplinkBytes != 0 {
		t.Fatalf("reused name did not create a fresh identity: %#v", reused.Identity)
	}
}

func TestSetAdminEnabledRespectsQuotaAndRejectsMismatchedReplay(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(100)
	record := presentUser(t, fixture, "Toggle", &limit)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestID := appID(t)
	if _, err := users.SetAdminEnabled(context.Background(), SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: requestID, ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	var conflict *domain.ConflictError
	if _, err := users.SetAdminEnabled(context.Background(), SetEnabledInput{ID: record.User.ID, Enabled: true, ExpectedRevision: 0, RequestID: requestID, ActorID: appID(t)}); !errors.As(err, &conflict) {
		t.Fatalf("mismatched replay error = %v", err)
	}
	if replay, err := users.SetAdminEnabled(context.Background(), SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: requestID, ActorID: appID(t)}); err != nil || !replay {
		t.Fatalf("identical replay = %v, %v", replay, err)
	}
	// 重新启用时用量仍超限：保持不可用，状态为配额超限。
	if _, err := users.SetAdminEnabled(context.Background(), SetEnabledInput{ID: record.User.ID, Enabled: true, ExpectedRevision: 1, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.DesiredPresent(after.User) || !after.Allocation.AdminEnabled || after.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatalf("re-enable over quota = %#v", after.Allocation)
	}
	if operationsFor(t, fixture, record.Allocation.ID, "enable") != 0 {
		t.Fatal("enable operation created although quota still blocks access")
	}
}
