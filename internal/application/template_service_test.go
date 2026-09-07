package application

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

type featureFixture struct {
	store    *sqlite.Store
	keyring  *security.Keyring
	clock    *ports.FixedClock
	adapter  *xrayfake.Adapter
	profiles *ProfileService
}

func appID(t *testing.T) domain.ID {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newFeatureFixture(t *testing.T) *featureFixture {
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
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{9}, 32))
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
	return &featureFixture{store: store, keyring: keyring, clock: clock, adapter: adapter,
		profiles: NewProfileService(store, adapter, keyring, clock, target, nil)}
}

func validProfileInput(t *testing.T) ProfileInput {
	t.Helper()
	key, err := security.GenerateUserKey(security.MethodAES256)
	if err != nil {
		t.Fatal(err)
	}
	return ProfileInput{Name: "Primary", InboundTag: "managed", PublicHost: "vpn.example.com", PublicPort: 8388,
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: key.Reveal(),
		BootstrapStatisticsID: "bootstrap", RequestID: appID(t), ActorID: appID(t)}
}

func registerCompatibleProfile(t *testing.T, fixture *featureFixture) domain.ID {
	t.Helper()
	input := validProfileInput(t)
	id, err := fixture.profiles.RegisterProfile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.profiles.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.store.Profile(context.Background(), id)
	if err != nil || record.Profile.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("profile = %#v, %v", record, err)
	}
	return id
}

func TestProfileRegistrationValidationAndBlankKeyUpdate(t *testing.T) {
	fixture := newFeatureFixture(t)
	input := validProfileInput(t)
	id, err := fixture.profiles.RegisterProfile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.Profile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.profiles.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	input.PublicHost = "new.example.com"
	input.ServerKey = ""
	input.RequestID = appID(t)
	input.ExpectedRevision = 0
	if err := fixture.profiles.UpdateProfile(context.Background(), id, input); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.store.Profile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.ServerKeyCiphertext, after.ServerKeyCiphertext) || !bytes.Equal(before.ServerKeyNonce, after.ServerKeyNonce) {
		t.Fatal("blank key input changed encrypted service key")
	}
	if after.Profile.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("public metadata edit reset compatibility: %s", after.Profile.Compatibility)
	}
}

func TestProfileIncompatibilityReasonIsSafe(t *testing.T) {
	fixture := newFeatureFixture(t)
	input := validProfileInput(t)
	fixture.adapter.Profiles[input.InboundTag] = ports.ProfileCapabilities{CompatibilityReason: "inbound is not multi-user"}
	id, err := fixture.profiles.RegisterProfile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.profiles.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.store.Profile(context.Background(), id)
	if err != nil || record.Profile.Compatibility != domain.CompatibilityIncompatible || record.Profile.CompatibilityReason == "" {
		t.Fatalf("profile = %#v, %v", record, err)
	}
	if bytes.Contains([]byte(record.Profile.CompatibilityReason), []byte(input.ServerKey)) {
		t.Fatal("compatibility reason leaked the server key")
	}
}

// leaseForTest 把操作直接置为 "test" 持有的租约，供不经 worker 的确认测试使用（ConfirmSync 要求租约 owner 匹配）。
func leaseForTest(t *testing.T, fixture *featureFixture, id domain.ID) {
	t.Helper()
	if _, err := fixture.store.DB().Write.Exec(`UPDATE synchronization_operations SET state='leased',lease_owner='test',lease_expires_at=? WHERE id=?`,
		fixture.clock.Now().Add(time.Minute).UnixMilli(), id.String()); err != nil {
		t.Fatal(err)
	}
}
