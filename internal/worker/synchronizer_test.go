package worker

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

type syncFixture struct {
	store   *sqlite.Store
	keyring *security.Keyring
	clock   *ports.FixedClock
	adapter *xrayfake.Adapter
	sync    *Synchronizer
	users   *application.UserService
	profile domain.ID
	tag     string
}

func fixtureID(t *testing.T) domain.ID {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "xpanel.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	clock := &ports.FixedClock{Time: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	verifier, nonce, err := keyring.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSingletons(ctx, "UTC", "127.0.0.1:10085", "v26.3.27", verifier, nonce, clock.Now()); err != nil {
		t.Fatal(err)
	}
	adapter := xrayfake.New()
	adapter.Now = clock.Now
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	profiles := application.NewProfileService(store, adapter, keyring, clock, target, nil)
	key, err := security.GenerateUserKey(security.MethodAES256)
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := profiles.RegisterProfile(ctx, application.ProfileInput{Name: "Primary", InboundTag: "managed",
		PublicHost: "vpn.example.com", PublicPort: 8388, Method: security.MethodAES256, Network: domain.NetworkTCPUDP,
		ServerKey: key.Reveal(), BootstrapStatisticsID: "bootstrap", RequestID: fixtureID(t), ActorID: fixtureID(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.RunValidation(ctx, profileID); err != nil {
		t.Fatal(err)
	}
	node := &sync.Mutex{}
	synchronizer := NewSynchronizer(store, adapter, keyring, clock, nil, node, SynchronizerOptions{
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, Random: func(n int64) int64 { return n - 1 }})
	users := application.NewUserService(store, keyring, clock, synchronizer.Wake)
	return &syncFixture{store: store, keyring: keyring, clock: clock, adapter: adapter, sync: synchronizer, users: users,
		profile: profileID, tag: "managed"}
}

func (f *syncFixture) createUser(t *testing.T, name string) ports.UserRecord {
	t.Helper()
	id, _, err := f.users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: name, ProfileID: f.profile,
		ResetDay: 1, RequestID: fixtureID(t), ActorID: fixtureID(t)})
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.store.User(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (f *syncFixture) calls(operation string) int {
	count := 0
	for _, call := range f.adapter.Calls {
		if call.Operation == operation {
			count++
		}
	}
	return count
}

func (f *syncFixture) operationState(t *testing.T, allocationID domain.ID) (string, int, time.Time) {
	t.Helper()
	var state string
	var attempts int
	var next int64
	if err := f.store.DB().Read.QueryRow(`SELECT state,attempt_count,next_attempt_at FROM synchronization_operations
        WHERE allocation_id=? ORDER BY desired_revision DESC LIMIT 1`, allocationID.String()).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	return state, attempts, time.UnixMilli(next).UTC()
}

func deadline(operation string) error {
	return &ports.AdapterError{Kind: ports.ErrorDeadlineExceeded, Operation: operation, Retryable: true, SafeSummary: "Xray operation timed out"}
}

func TestSynchronizerConfirmsCreate(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	if processed, err := f.sync.Drain(context.Background()); err != nil || processed != 1 {
		t.Fatalf("drain = %d, %v", processed, err)
	}
	after, err := f.store.User(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Allocation.ProjectionState != domain.ProjectionPresent || after.Allocation.SyncedRevision != 1 ||
		after.Credential.State != domain.CredentialActive || after.Allocation.PendingSync() {
		t.Fatalf("allocation after sync = %#v credential=%s", after.Allocation, after.Credential.State)
	}
	if _, ok := f.adapter.Users[f.tag][record.Identity.StatisticsID]; !ok {
		t.Fatal("fake Xray does not contain the user")
	}
	state, _, _ := f.operationState(t, record.Allocation.ID)
	if state != string(domain.SyncSucceeded) {
		t.Fatalf("operation state = %s", state)
	}
	var audits int
	if err := f.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND target_id=?`,
		domain.ActionSyncSucceeded, record.User.ID.String()).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count = %d, %v", audits, err)
	}
}

func TestSynchronizerTimeoutThenPresentConfirmsWithoutDuplicate(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Err: deadline("add_user"), Applied: true}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := f.store.User(context.Background(), record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPresent || f.calls("add_user") != 1 || f.calls("list_users") != 1 {
		t.Fatalf("read-after-write did not confirm: %s add=%d list=%d", after.Allocation.ProjectionState, f.calls("add_user"), f.calls("list_users"))
	}
}

func TestSynchronizerTimeoutThenAbsentRetriesWithBackoff(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Err: deadline("add_user")}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, attempts, next := f.operationState(t, record.Allocation.ID)
	if state != string(domain.SyncRetryWait) || attempts != 1 || !next.After(f.clock.Now()) {
		t.Fatalf("retry state = %s attempts=%d next=%s now=%s", state, attempts, next, f.clock.Now())
	}
	after, _ := f.store.User(context.Background(), record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPending || after.Allocation.LastSyncErrorCode != ports.ErrorDeadlineExceeded {
		t.Fatalf("pending allocation = %#v", after.Allocation)
	}
	if processed, _ := f.sync.Drain(context.Background()); processed != 0 {
		t.Fatal("operation was retried before its backoff elapsed")
	}
	f.clock.Set(next)
	if processed, err := f.sync.Drain(context.Background()); err != nil || processed != 1 {
		t.Fatalf("drain after backoff = %d, %v", processed, err)
	}
	after, _ = f.store.User(context.Background(), record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPresent || after.Allocation.LastSyncErrorCode != "" {
		t.Fatalf("allocation after retry = %#v", after.Allocation)
	}
}

func TestSynchronizerBackoffIsBoundedAndInstanceMarkedUnreachable(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	f.adapter.Available = false
	var previous time.Duration
	for attempt := 1; attempt <= 8; attempt++ {
		if _, err := f.sync.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, attempts, next := f.operationState(t, record.Allocation.ID)
		if state != string(domain.SyncRetryWait) || attempts != attempt {
			t.Fatalf("attempt %d state=%s attempts=%d", attempt, state, attempts)
		}
		wait := next.Sub(f.clock.Now())
		if wait <= 0 || wait > 30*time.Second || wait < previous && previous < 30*time.Second {
			t.Fatalf("attempt %d backoff %s (previous %s) out of bounds", attempt, wait, previous)
		}
		previous = wait
		f.clock.Set(next)
	}
	instance, err := f.store.ManagedInstance(context.Background())
	if err != nil || instance.HealthState != "unreachable" {
		t.Fatalf("instance = %#v, %v", instance, err)
	}
	profile, _ := f.store.Profile(context.Background(), f.profile)
	if profile.Profile.Compatibility != domain.CompatibilityUnreachable {
		t.Fatalf("profile compatibility = %s", profile.Profile.Compatibility)
	}
	f.adapter.Available = true
	recovered := 0
	f.sync.onProfileRecovered = func(domain.ID) { recovered++ }
	_, _, next := f.operationState(t, record.Allocation.ID)
	f.clock.Set(next)
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := f.store.User(context.Background(), record.User.ID)
	instance, _ = f.store.ManagedInstance(context.Background())
	if after.Allocation.ProjectionState != domain.ProjectionPresent || instance.HealthState != "healthy" || recovered != 1 {
		t.Fatalf("recovery: projection=%s health=%s recovered=%d", after.Allocation.ProjectionState, instance.HealthState, recovered)
	}
}

func TestSynchronizerRepairsAlreadyExists(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	f.adapter.Users[f.tag] = map[string]ports.RemoteUser{record.Identity.StatisticsID: {StatisticsID: record.Identity.StatisticsID, Present: true, Kind: "managed", CredentialVersion: 99}}
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUserAlreadyExists, Operation: "add_user", SafeSummary: "Xray user already exists"}}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls("remove_user") != 1 || f.calls("add_user") != 2 {
		t.Fatalf("repair calls remove=%d add=%d", f.calls("remove_user"), f.calls("add_user"))
	}
	remote := f.adapter.Users[f.tag][record.Identity.StatisticsID]
	after, _ := f.store.User(context.Background(), record.User.ID)
	if remote.CredentialVersion != 1 || after.Allocation.ProjectionState != domain.ProjectionPresent {
		t.Fatalf("repair result remote=%#v projection=%s", remote, after.Allocation.ProjectionState)
	}
}

func TestSynchronizerNonRetryableFailsPermanentlyAndMarksProfile(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	f.adapter.Failures["add_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorIncompatibleProfile, Operation: "add_user", SafeSummary: "inbound does not support dynamic users"}}}
	if _, err := f.sync.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _, _ := f.operationState(t, record.Allocation.ID)
	profile, _ := f.store.Profile(context.Background(), f.profile)
	after, _ := f.store.User(context.Background(), record.User.ID)
	if state != string(domain.SyncPermanentFailed) || profile.Profile.Compatibility != domain.CompatibilityIncompatible ||
		after.Allocation.ProjectionState != domain.ProjectionError {
		t.Fatalf("state=%s profile=%s projection=%s", state, profile.Profile.Compatibility, after.Allocation.ProjectionState)
	}
	var audits int
	_ = f.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, domain.ActionSyncFailed).Scan(&audits)
	if audits != 1 {
		t.Fatalf("sync_failed audit count = %d", audits)
	}
}

func TestSynchronizerSupersedesOlderRevision(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	ctx := context.Background()
	if _, err := f.store.DB().Write.Exec(`UPDATE access_allocations SET desired_revision=2,admin_enabled=0 WHERE id=?`, record.Allocation.ID.String()); err != nil {
		t.Fatal(err)
	}
	disable := domain.NewSynchronizationOperation(fixtureID(t), record.Allocation.ID, 2, false, nil, domain.SyncDisable, domain.SyncRemoveOld, f.clock.Now())
	if err := f.store.Enqueue(ctx, disable); err != nil {
		t.Fatal(err)
	}
	var oldState string
	_ = f.store.DB().Read.QueryRow(`SELECT state FROM synchronization_operations WHERE desired_revision=1`).Scan(&oldState)
	if oldState != string(domain.SyncSuperseded) {
		t.Fatalf("older operation state = %s", oldState)
	}
	if processed, err := f.sync.Drain(ctx); err != nil || processed != 1 {
		t.Fatalf("drain = %d, %v", processed, err)
	}
	if f.calls("add_user") != 0 || f.calls("remove_user") != 1 {
		t.Fatalf("superseded add ran: add=%d remove=%d", f.calls("add_user"), f.calls("remove_user"))
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionAbsent || after.Allocation.SyncedRevision != 2 {
		t.Fatalf("allocation after disable = %#v", after.Allocation)
	}
}

func TestSynchronizerResumesExpiredLeaseAfterRestart(t *testing.T) {
	f := newSyncFixture(t)
	record := f.createUser(t, "Alice")
	ctx := context.Background()
	if work, err := f.store.LeaseDueSync(ctx, "crashed-worker", f.clock.Now(), time.Second); err != nil || work == nil {
		t.Fatalf("lease = %#v, %v", work, err)
	}
	if processed, _ := f.sync.Drain(ctx); processed != 0 {
		t.Fatal("live lease was stolen")
	}
	f.clock.Advance(2 * time.Second)
	if processed, err := f.sync.Drain(ctx); err != nil || processed != 1 {
		t.Fatalf("drain after lease expiry = %d, %v", processed, err)
	}
	after, _ := f.store.User(ctx, record.User.ID)
	if after.Allocation.ProjectionState != domain.ProjectionPresent {
		t.Fatalf("projection = %s", after.Allocation.ProjectionState)
	}
}

func TestSynchronizerRunWakesOnNotification(t *testing.T) {
	f := newSyncFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.sync.Run(ctx); close(done) }()
	record := f.createUser(t, "Alice") // CreateUser calls Wake through the notify hook.
	deadlineAt := time.Now().Add(5 * time.Second)
	for {
		after, err := f.store.User(context.Background(), record.User.ID)
		if err == nil && after.Allocation.ProjectionState == domain.ProjectionPresent {
			break
		}
		if time.Now().After(deadlineAt) {
			t.Fatal("synchronizer did not process the woken operation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("synchronizer did not stop")
	}
}

func TestDescribeFallsBackToInternal(t *testing.T) {
	kind, retryable := describe(errors.New("boom"))
	if kind != ports.ErrorInternal || !retryable {
		t.Fatalf("describe = %s %v", kind, retryable)
	}
}
