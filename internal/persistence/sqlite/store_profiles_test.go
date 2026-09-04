package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

func fixtureID(t *testing.T) domain.ID {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedProfile(t *testing.T, store *Store, now time.Time) ports.ProfileRecord {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureSingletons(ctx, "UTC", "127.0.0.1:10085", "v26.3.27", []byte{1}, []byte{2}, now); err != nil {
		t.Fatal(err)
	}
	instance, err := store.ManagedInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := domain.NewAccessProfile(fixtureID(t), instance.ID, "Primary", "ss2022", "vpn.example.com", 8388,
		security.MethodAES256, domain.NetworkTCPUDP, "bootstrap-primary", now)
	if err != nil {
		t.Fatal(err)
	}
	record := ports.ProfileRecord{Profile: profile, ServerKeyCiphertext: []byte("ciphertext"), ServerKeyNonce: []byte("nonce"), KeyEncryptionVersion: 1}
	commandID, auditID := fixtureID(t), fixtureID(t)
	actor := fixtureID(t)
	command := domain.DomainCommand{ID: commandID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionProfileRegistered, TargetType: "profile", TargetID: profile.ID,
		RequestFingerprint: domain.Fingerprint("profile", profile.ID.String()), State: domain.CommandCompleted,
		ResultReference: "/profiles/" + profile.ID.String(), CreatedAt: now, CompletedAt: &now}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "profile", TargetID: profile.ID, Action: domain.ActionProfileRegistered,
		Result: domain.AuditSucceeded, CommandID: &commandID, SafeSummary: "profile registered"}
	if err := store.CreateProfile(ctx, record, command, audit); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestProfileRevisionArchiveAndIdentityUniqueness(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := seedProfile(t, store, now)

	loaded, err := store.Profile(context.Background(), record.Profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.ServerKeyCiphertext) != "ciphertext" || loaded.Profile.Compatibility != domain.CompatibilityUnverified {
		t.Fatalf("loaded profile = %#v", loaded)
	}
	identity := domain.XrayUserIdentity{ID: fixtureID(t), InstanceID: record.Profile.InstanceID, ProfileID: record.Profile.ID,
		StatisticsID: record.Profile.BootstrapStatisticsID, Kind: domain.IdentityBootstrap, CreatedAt: now}
	if err := store.RegisterBootstrapIdentity(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	// Registration is idempotent for the same instance/statistics identity.
	identity.ID = fixtureID(t)
	if err := store.RegisterBootstrapIdentity(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveProfile(context.Background(), record.Profile.ID, record.Profile.Revision, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	profiles, err := store.Profiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("archived profile remained visible: %#v", profiles)
	}
	var conflict *domain.ConflictError
	if err := store.ArchiveProfile(context.Background(), record.Profile.ID, 0, now); !errors.As(err, &conflict) {
		t.Fatalf("stale archive error = %v", err)
	}
}
