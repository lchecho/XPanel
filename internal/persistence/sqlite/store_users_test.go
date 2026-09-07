package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func userFixture(t *testing.T, store *Store, now time.Time, name string) ports.UserCreateRecord {
	t.Helper()
	templates, err := store.Templates(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var template ports.TemplateRecord
	if len(templates) == 0 {
		template = seedTemplate(t, store, now)
	} else {
		template = templates[0]
	}
	if err := store.SetTemplateCompatibility(context.Background(), template.Template.ID, domain.CompatibilityCompatible, "", now); err != nil {
		t.Fatal(err)
	}
	user, err := domain.NewManagedUser(fixtureID(t), name, now)
	if err != nil {
		t.Fatal(err)
	}
	allocationID := fixtureID(t)
	identity := domain.XrayUserIdentity{ID: fixtureID(t), InstanceID: template.Template.InstanceID, TemplateID: template.Template.ID,
		StatisticsID: "xpanel-" + allocationID.String(), Kind: domain.IdentityManaged, CreatedAt: now}
	version := int64(1)
	allocation := domain.AccessAllocation{ID: allocationID, UserID: user.ID, TemplateID: template.Template.ID, IdentityID: identity.ID,
		AdminEnabled: true, QuotaState: domain.QuotaWithinLimit, ProjectionState: domain.ProjectionPending,
		DesiredRevision: 1, DesiredCredentialVersion: version, CreatedAt: now, UpdatedAt: now}
	credential := domain.AccessCredential{ID: fixtureID(t), AllocationID: allocationID, Version: version,
		State: domain.CredentialPending, KeyCiphertext: []byte("user-cipher"), KeyNonce: []byte("user-nonce"),
		KeyEncryptionVersion: 1, CreatedAt: now}
	policy := ports.QuotaPolicyRecord{AllocationID: allocationID, ResetDay: 1, CreatedAt: now, UpdatedAt: now}
	cycle := ports.QuotaCycleRecord{ID: fixtureID(t), AllocationID: allocationID, StartsAt: now.Add(-time.Hour),
		EndsAt: now.AddDate(0, 1, 0), Timezone: "UTC", ResetDay: 1, Status: "open", OpenedAt: now}
	inbound := ports.InboundRecord{Inbound: domain.DedicatedInbound{AllocationID: allocationID, TemplateID: template.Template.ID,
		InboundTag: domain.InboundTag(allocationID), ListenAddress: template.Template.ListenAddress,
		Port: nextFixturePort(t, store, template.Template.ID), DesiredPresent: true, CreatedAt: now, UpdatedAt: now},
		ServerKeyCiphertext: []byte("server-cipher"), ServerKeyNonce: []byte("server-nonce"), KeyEncryptionVersion: 1}
	operation := domain.NewSynchronizationOperation(fixtureID(t), allocationID, 1, true, &version, domain.SyncCreate, domain.SyncCreateInbound, now)
	commandID, auditID, actor := fixtureID(t), fixtureID(t), fixtureID(t)
	command := domain.DomainCommand{ID: commandID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionUserCreated, TargetType: "user", TargetID: user.ID,
		RequestFingerprint: domain.Fingerprint("create", user.NormalizedName), State: domain.CommandCompleted,
		ResultReference: "/users/" + user.ID.String(), CreatedAt: now, CompletedAt: &now}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "user", TargetID: user.ID, Action: domain.ActionUserCreated, Result: domain.AuditSucceeded,
		CommandID: &commandID, OperationID: &operation.ID, SafeSummary: "user created"}
	portAudit := domain.AuditEvent{ID: fixtureID(t), OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "user", TargetID: user.ID, Action: domain.ActionPortAssigned, Result: domain.AuditSucceeded,
		CommandID: &commandID, OperationID: &operation.ID, SafeSummary: "port assigned"}
	return ports.UserCreateRecord{User: user, Identity: identity, Allocation: allocation, Inbound: inbound, Credential: credential,
		Policy: policy, Cycle: cycle, Operation: operation, Command: command, Audit: audit, PortAudit: portAudit}
}

func TestCreateUserIsAtomicAndReplayable(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := userFixture(t, store, now, "Alice")
	id, replay, err := store.CreateUser(context.Background(), record)
	if err != nil || replay || id != record.User.ID {
		t.Fatalf("create = %s, %v, %v", id, replay, err)
	}
	loaded, err := store.User(context.Background(), id)
	if err != nil || loaded.Allocation.UserID != id || loaded.Credential.State != domain.CredentialPending {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	if _, replay, err := store.CreateUser(context.Background(), record); err != nil || !replay {
		t.Fatalf("replay = %v, %v", replay, err)
	}

	duplicate := userFixture(t, store, now.Add(time.Second), "Alice")
	if _, _, err := store.CreateUser(context.Background(), duplicate); err == nil {
		t.Fatal("duplicate active name was accepted")
	}

	failed := userFixture(t, store, now.Add(2*time.Second), "Bob")
	failed.Audit.ID = record.Audit.ID // final insert fails after every aggregate was staged.
	if _, _, err := store.CreateUser(context.Background(), failed); err == nil {
		t.Fatal("fault injection unexpectedly succeeded")
	}
	if _, err := store.User(context.Background(), failed.User.ID); err == nil {
		t.Fatal("partially committed user remained after failure")
	} else {
		var notFound *domain.NotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("lookup after rollback = %v", err)
		}
	}
}

// nextFixturePort 为夹具在模板端口池内取一个未占用端口，保证多用户夹具不会撞端口。
func nextFixturePort(t *testing.T, store *Store, templateID domain.ID) int {
	t.Helper()
	assigned, err := store.AssignedPorts(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	template, err := store.Template(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	port, err := domain.NextAvailablePort(template.Template.Pool, assigned)
	if err != nil {
		t.Fatal(err)
	}
	return port
}
