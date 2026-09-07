package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

type memoryAuthStore struct {
	admin   *ports.AdministratorRecord
	audits  []domain.AuditEvent
	revoked bool
}

type memoryAuthTx struct{ store *memoryAuthStore }

func (s *memoryAuthStore) WithWriteTx(_ context.Context, fn func(ports.WriteTx) error) error {
	return fn(&memoryAuthTx{s})
}
func (s *memoryAuthStore) WithReadTx(_ context.Context, fn func(ports.ReadTx) error) error {
	return fn(&memoryAuthTx{s})
}
func (*memoryAuthStore) Close() error { return nil }
func (*memoryAuthTx) FindCommand(context.Context, domain.ID) (*domain.DomainCommand, error) {
	return nil, nil
}
func (*memoryAuthTx) PanelSettings(context.Context) (ports.PanelSettingsRecord, error) {
	return ports.PanelSettingsRecord{}, nil
}
func (t *memoryAuthTx) Administrator(context.Context) (*ports.AdministratorRecord, error) {
	return t.store.admin, nil
}
func (*memoryAuthTx) SaveCommand(context.Context, domain.DomainCommand) error { return nil }
func (t *memoryAuthTx) AppendAudit(_ context.Context, event domain.AuditEvent) error {
	t.store.audits = append(t.store.audits, event)
	return nil
}
func (t *memoryAuthTx) CreateAdministrator(_ context.Context, admin ports.AdministratorRecord) error {
	copy := admin
	t.store.admin = &copy
	return nil
}
func (t *memoryAuthTx) UpdateAdministratorPassword(_ context.Context, _ domain.ID, hash string, version int64, now time.Time) error {
	t.store.admin.PasswordHash = hash
	t.store.admin.PasswordVersion = version
	t.store.admin.UpdatedAt = now
	return nil
}
func (t *memoryAuthTx) RevokeAllSessions(context.Context, domain.ID, time.Time) error {
	t.store.revoked = true
	return nil
}
func (t *memoryAuthTx) InsertSession(context.Context, ports.SessionRecord) error { return nil }
func (t *memoryAuthTx) RevokeSession(context.Context, []byte, time.Time) (bool, error) {
	t.store.revoked = true
	return true, nil
}

func TestInitializeLoginAndReset(t *testing.T) {
	clock := &ports.FixedClock{Time: time.Now()}
	store := &memoryAuthStore{}
	service, err := NewAuthService(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	id, err := service.InitializeAdministrator(context.Background(), "Admin", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InitializeAdministrator(context.Background(), "other", []byte("another safe password")); err == nil {
		t.Fatal("second initialization succeeded")
	}
	admin, err := service.Login(context.Background(), "ADMIN", []byte("correct horse battery staple"), "127.0.0.1")
	if err != nil || admin.ID != id {
		t.Fatalf("login = %#v, %v", admin, err)
	}
	if _, err := service.Login(context.Background(), "admin", []byte("wrong password value"), "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v", err)
	}
	if err := service.ResetPassword(context.Background(), []byte("replacement password value")); err != nil {
		t.Fatal(err)
	}
	if !store.revoked || store.admin.PasswordVersion != 2 {
		t.Fatal("reset did not revoke sessions and increment version")
	}
}

func TestLoginThrottle(t *testing.T) {
	clock := &ports.FixedClock{Time: time.Now()}
	store := &memoryAuthStore{}
	service, err := NewAuthService(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.InitializeAdministrator(context.Background(), "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		_, err := service.Login(context.Background(), "admin", []byte("wrong password value"), "127.0.0.1")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err = service.Login(context.Background(), "admin", []byte("wrong password value"), "127.0.0.1")
	var limited *domain.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("expected rate limit, got %v", err)
	}
	clock.Advance(16 * time.Minute)
	_, err = service.Login(context.Background(), "admin", []byte("correct horse battery staple"), "127.0.0.1")
	if err != nil {
		t.Fatalf("login after window: %v", err)
	}
}
