package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "xpanel.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenVerifiesPragmasAndExclusiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xpanel.db")
	db, err := Open(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Open(context.Background(), path, 5*time.Second); err == nil {
		t.Fatal("second writer acquired the lock")
	}
}

func TestReadContinuesDuringWriteTransaction(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(context.Background(), db.Write); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO administrators(id,username,password_hash,password_version,created_at,updated_at)
        VALUES ('a','admin','hash',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read.QueryRow(`SELECT count(*) FROM administrators`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("uncommitted data became visible: %d", count)
	}
}
