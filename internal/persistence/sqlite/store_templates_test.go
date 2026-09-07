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

func seedTemplate(t *testing.T, store *Store, now time.Time) ports.TemplateRecord {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureSingletons(ctx, "UTC", "127.0.0.1:10085", "v26.3.27", []byte{1}, []byte{2}, now); err != nil {
		t.Fatal(err)
	}
	instance, err := store.ManagedInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	template, err := domain.NewInboundTemplate(fixtureID(t), instance.ID, "Primary", "vpn.example.com", "127.0.0.1",
		30000, 30099, security.MethodAES256, domain.NetworkTCPUDP, now)
	if err != nil {
		t.Fatal(err)
	}
	record := ports.TemplateRecord{Template: template}
	commandID, auditID := fixtureID(t), fixtureID(t)
	actor := fixtureID(t)
	command := domain.DomainCommand{ID: commandID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionTemplateRegistered, TargetType: "template", TargetID: template.ID,
		RequestFingerprint: domain.Fingerprint("template", template.ID.String()), State: domain.CommandCompleted,
		ResultReference: "/templates/" + template.ID.String(), CreatedAt: now, CompletedAt: &now}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "template", TargetID: template.ID, Action: domain.ActionTemplateRegistered,
		Result: domain.AuditSucceeded, CommandID: &commandID, SafeSummary: "template registered"}
	if _, _, err := store.CreateTemplate(ctx, record, command, audit); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Template(ctx, template.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// 入站模板的登记、重放幂等与端口池读写。
func TestTemplateCreateReplayAndPortPool(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := seedTemplate(t, store, now)
	if record.Template.Pool.Start != 30000 || record.Template.Pool.End != 30099 || record.Template.Pool.Capacity() != 100 {
		t.Fatalf("pool = %#v", record.Template.Pool)
	}
	if record.Template.ListenAddress != "127.0.0.1" || record.Template.Compatibility != domain.CompatibilityUnverified {
		t.Fatalf("template = %#v", record.Template)
	}
	templates, err := store.Templates(context.Background(), false)
	if err != nil || len(templates) != 1 {
		t.Fatalf("templates = %d, %v", len(templates), err)
	}
	usage, err := store.PortPoolUsage(context.Background(), record.Template.ID)
	if err != nil || usage.Capacity != 100 || usage.Assigned != 0 || usage.Remaining != 100 {
		t.Fatalf("usage = %#v, %v", usage, err)
	}
}

// 契约字段（method / listen_address）在存在用户时不得变更；端口池可随时调整。
func TestTemplateContractFieldsFrozenWhileUsersExist(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := userFixture(t, store, now, "Alice")
	if _, _, err := store.CreateUser(ctx, record); err != nil {
		t.Fatal(err)
	}
	template, err := store.Template(ctx, record.Allocation.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	changed := template
	changed.Template.Method = security.MethodAES128
	changed.Template.UpdatedAt = now
	command, audit := updateCommand(t, template.Template.ID, now)
	var conflict *domain.ConflictError
	if err := store.UpdateTemplate(ctx, changed, ports.RevisionMatch{Expected: template.Template.Revision}, command, audit); !errors.As(err, &conflict) {
		t.Fatalf("method change while users exist err = %v", err)
	}
	// 端口池调整不受限制，即使既有分配落到池外。
	widened := template
	widened.Template.Pool = domain.PortPool{Start: 40000, End: 40010}
	widened.Template.UpdatedAt = now
	command2, audit2 := updateCommand(t, template.Template.ID, now)
	if err := store.UpdateTemplate(ctx, widened, ports.RevisionMatch{Expected: template.Template.Revision}, command2, audit2); err != nil {
		t.Fatalf("port pool change rejected: %v", err)
	}
	usage, err := store.PortPoolUsage(ctx, template.Template.ID)
	if err != nil || len(usage.Outside) != 1 {
		t.Fatalf("usage after shrink = %#v, %v", usage, err)
	}
}

func updateCommand(t *testing.T, id domain.ID, now time.Time) (domain.DomainCommand, domain.AuditEvent) {
	t.Helper()
	commandID, auditID, actor := fixtureID(t), fixtureID(t), fixtureID(t)
	command := domain.DomainCommand{ID: commandID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionTemplateUpdated, TargetType: "template", TargetID: id,
		RequestFingerprint: domain.Fingerprint("update", commandID.String()), State: domain.CommandCompleted,
		ResultReference: "/templates/" + id.String(), CreatedAt: now, CompletedAt: &now}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "template", TargetID: id, Action: domain.ActionTemplateUpdated, Result: domain.AuditSucceeded,
		CommandID: &commandID, SafeSummary: "template updated"}
	return command, audit
}
