package sqlite

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func TestSessionExpiryRevocationAndPasswordVersion(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	adminID, _ := domain.NewID()
	if err := store.WithWriteTx(context.Background(), func(tx ports.WriteTx) error {
		return tx.CreateAdministrator(context.Background(), ports.AdministratorRecord{ID: adminID, Username: "admin", PasswordHash: "hash", PasswordVersion: 1, CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionStore(store.db, 30*time.Minute, 12*time.Hour)
	sessions.now = func() time.Time { return now }
	if err := sessions.Commit("token", []byte("data"), now.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, found, err := sessions.Find("token")
	if err != nil || !found || string(data) != "data" {
		t.Fatalf("find = %q, %v, %v", data, found, err)
	}
	if err := sessions.Commit("expiring", []byte("data"), now.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	sessions.now = func() time.Time { return now.Add(31 * time.Minute) }
	if _, found, _ := sessions.Find("expiring"); found {
		t.Fatal("idle-expired session remained valid")
	}
	sessions.now = func() time.Time { return now }
	if err := sessions.RevokeAll(adminID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := sessions.Find("token"); found {
		t.Fatal("revoked session remained valid")
	}
	if err := sessions.Commit("token2", []byte("data"), now.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Write.Exec(`UPDATE administrators SET password_version=2 WHERE id=?`, adminID.String()); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := sessions.Find("token2"); found {
		t.Fatal("old password-version session remained valid")
	}
}
