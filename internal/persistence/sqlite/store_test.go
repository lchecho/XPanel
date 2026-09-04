package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := openTestDB(t)
	if err := Migrate(context.Background(), db.Write); err != nil {
		t.Fatal(err)
	}
	return NewStore(db)
}

func TestCommandIdempotencyAndConflict(t *testing.T) {
	store := newTestStore(t)
	id, _ := domain.NewID()
	target, _ := domain.NewID()
	command := domain.DomainCommand{ID: id, ActorType: domain.ActorSystem, CommandType: "test", TargetType: "test",
		TargetID: target, RequestFingerprint: domain.Fingerprint("one"), State: domain.CommandAccepted, CreatedAt: time.Now()}
	if err := store.WithWriteTx(context.Background(), func(tx ports.WriteTx) error { return tx.SaveCommand(context.Background(), command) }); err != nil {
		t.Fatal(err)
	}
	if err := store.WithWriteTx(context.Background(), func(tx ports.WriteTx) error { return tx.SaveCommand(context.Background(), command) }); err != nil {
		t.Fatal(err)
	}
	command.RequestFingerprint = domain.Fingerprint("two")
	err := store.WithWriteTx(context.Background(), func(tx ports.WriteTx) error { return tx.SaveCommand(context.Background(), command) })
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestEnsureSingletonsDoesNotOverwriteTimezone(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	if err := store.EnsureSingletons(context.Background(), "UTC", "127.0.0.1:10085", "v26.3.27", []byte{1}, []byte{2}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Write.Exec(`UPDATE panel_settings SET quota_timezone='Asia/Tokyo',revision=1`); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSingletons(context.Background(), "America/New_York", "127.0.0.1:10085", "v26.3.27", []byte{3}, []byte{4}, now); err != nil {
		t.Fatal(err)
	}
	var timezone string
	if err := store.db.Read.QueryRow(`SELECT quota_timezone FROM panel_settings WHERE id=1`).Scan(&timezone); err != nil {
		t.Fatal(err)
	}
	if timezone != "Asia/Tokyo" {
		t.Fatalf("timezone overwritten: %s", timezone)
	}
}

func TestAuditIsAppendOnlyThroughStoreAPI(t *testing.T) {
	store := newTestStore(t)
	if _, ok := any(store).(interface{ UpdateAudit(context.Context) error }); ok {
		t.Fatal("store exposes an audit update operation")
	}
}
