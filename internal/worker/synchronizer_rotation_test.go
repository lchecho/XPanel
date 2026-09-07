package worker

import (
	"context"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
)

func (f *syncFixture) rotate(t *testing.T, userID domain.ID, revision domain.Revision) {
	t.Helper()
	if _, err := f.users.RotateCredential(context.Background(), application.LifecycleInput{ID: userID, ExpectedRevision: revision,
		RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
}

func (f *syncFixture) operationPhase(t *testing.T, allocationID domain.ID, revision domain.Revision) (string, string) {
	t.Helper()
	var phase, state string
	if err := f.store.DB().Read.QueryRow(`SELECT phase,state FROM synchronization_operations WHERE allocation_id=? AND desired_revision=?`,
		allocationID.String(), revision).Scan(&phase, &state); err != nil {
		t.Fatal(err)
	}
	return phase, state
}

func TestSynchronizerRotatesThroughRemoveAddConfirm(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Rotate")
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	f.rotate(t, record.User.ID, 0)
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	remote := f.adapter.Users[f.tagOf(record)][record.Identity.StatisticsID]
	// 轮换在既有入站内先删后加同一统计标识；入站只在创建时建立一次，端口不中断（FR-017）。
	if remote.CredentialVersion != 2 || f.calls("remove_user") != 1 || f.calls("add_user") != 1 ||
		f.calls("create_inbound") != 1 || f.calls("remove_inbound") != 0 {
		t.Fatalf("rotation calls remote=%#v remove_user=%d add_user=%d create_inbound=%d remove_inbound=%d",
			remote, f.calls("remove_user"), f.calls("add_user"), f.calls("create_inbound"), f.calls("remove_inbound"))
	}
	if !f.adapter.Listening(record.Inbound.Inbound.Port) {
		t.Fatal("rotation interrupted the dedicated port")
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Credential.Version != 2 || after.Credential.State != domain.CredentialActive || after.Allocation.SyncedRevision != 2 || after.Allocation.PendingSync() {
		t.Fatalf("after rotation = credential v%d %s allocation=%#v", after.Credential.Version, after.Credential.State, after.Allocation)
	}
	phase, state := f.operationPhase(t, record.Allocation.ID, 2)
	if phase != string(domain.SyncDone) || state != string(domain.SyncSucceeded) {
		t.Fatalf("rotation operation = %s/%s", phase, state)
	}
	var destroyed int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM access_credentials WHERE allocation_id=? AND state='destroyed' AND key_ciphertext IS NULL`, record.Allocation.ID.String()).Scan(&destroyed)
	if destroyed != 1 {
		t.Fatalf("destroyed credentials = %d", destroyed)
	}
}

func TestSynchronizerResumesRotationFromPersistedPhase(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Resume")
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	f.rotate(t, record.User.ID, 0)
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Err: deadline("add_user")}}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	phase, state := f.operationPhase(t, record.Allocation.ID, 2)
	if phase != string(domain.SyncAddDesired) || state != string(domain.SyncRetryWait) {
		t.Fatalf("after failed add phase=%s state=%s", phase, state)
	}
	if present := f.userPresent(record); present {
		t.Fatal("old credential still present after remove_old phase")
	}
	_, _, next := f.operationState(t, record.Allocation.ID)
	f.clock.Set(next)
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls("remove_user") != 1 {
		t.Fatalf("remove_old was repeated after phase advanced: %d", f.calls("remove_user"))
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Credential.State != domain.CredentialActive || after.Credential.Version != 2 || f.adapter.Users[f.tagOf(record)][record.Identity.StatisticsID].CredentialVersion != 2 {
		t.Fatalf("resumed rotation = credential v%d %s", after.Credential.Version, after.Credential.State)
	}
}

func TestSynchronizerDisableMidRotationSupersedesAndLaterEnableActivatesNewKey(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	record := f.createUser(t, "Mid")
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	f.rotate(t, record.User.ID, 0)
	if _, err := f.users.SetAdminEnabled(ctx, application.SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 1,
		RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	if _, state := f.operationPhase(t, record.Allocation.ID, 2); state != string(domain.SyncSuperseded) {
		t.Fatalf("rotation operation state after disable = %s", state)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionAbsent || after.Credential.State != domain.CredentialPending || after.Credential.Version != 2 {
		t.Fatalf("after disable mid-rotation = %#v credential v%d %s", after.Allocation, after.Credential.Version, after.Credential.State)
	}
	if _, err := f.users.SetAdminEnabled(ctx, application.SetEnabledInput{ID: record.User.ID, Enabled: true, ExpectedRevision: 2,
		RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ = f.store.User(ctx, record.User.ID)
	if after.Credential.State != domain.CredentialActive || f.adapter.Users[f.tagOf(record)][record.Identity.StatisticsID].CredentialVersion != 2 {
		t.Fatalf("re-enable did not activate the pending credential: %s", after.Credential.State)
	}
	_ = time.Second
}
