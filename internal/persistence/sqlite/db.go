package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type DB struct {
	Write *sql.DB
	Read  *sql.DB
	lock  *os.File
	path  string
}

func Open(ctx context.Context, path string, busyTimeout time.Duration) (*DB, error) {
	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN); _ = lock.Close() }

	dsn := sqliteDSN(path, busyTimeout)
	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("open SQLite writer: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	if err := write.PingContext(ctx); err != nil {
		write.Close()
		cleanup()
		return nil, fmt.Errorf("ping SQLite writer: %w", err)
	}
	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		write.Close()
		cleanup()
		return nil, fmt.Errorf("open SQLite reader: %w", err)
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(2)
	if err := read.PingContext(ctx); err != nil {
		read.Close()
		write.Close()
		cleanup()
		return nil, fmt.Errorf("ping SQLite reader: %w", err)
	}
	db := &DB{Write: write, Read: read, lock: lock, path: path}
	if err := db.verifyPragmas(ctx, busyTimeout); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.secureFiles(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func sqliteDSN(path string, busyTimeout time.Duration) string {
	values := url.Values{}
	values.Add("_pragma", "journal_mode(WAL)")
	values.Add("_pragma", "foreign_keys(ON)")
	values.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	values.Add("_pragma", "synchronous(FULL)")
	return "file:" + filepath.ToSlash(path) + "?" + values.Encode()
}

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("create single-writer lock")
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, errors.New("secure single-writer lock")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another XPanel writer already holds the database lock")
	}
	return f, nil
}

func (db *DB) verifyPragmas(ctx context.Context, busyTimeout time.Duration) error {
	wants := []struct {
		name string
		want string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", strconv.FormatInt(busyTimeout.Milliseconds(), 10)},
		{"synchronous", "2"},
	}
	for _, handle := range []*sql.DB{db.Write, db.Read} {
		for _, item := range wants {
			var got string
			if err := handle.QueryRowContext(ctx, "PRAGMA "+item.name).Scan(&got); err != nil {
				return fmt.Errorf("read PRAGMA %s: %w", item.name, err)
			}
			if got != item.want {
				return fmt.Errorf("PRAGMA %s=%s, want %s", item.name, got, item.want)
			}
		}
	}
	return nil
}

func (db *DB) secureFiles() error {
	for _, suffix := range []string{"", "-wal", "-shm", ".lock"} {
		path := db.path + suffix
		if _, err := os.Stat(path); err == nil {
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("secure SQLite file: %w", err)
			}
		}
	}
	return nil
}

func (db *DB) Close() error {
	var result error
	if db.Read != nil {
		result = errors.Join(result, db.Read.Close())
	}
	if db.Write != nil {
		result = errors.Join(result, db.Write.Close())
	}
	if db.lock != nil {
		_ = unix.Flock(int(db.lock.Fd()), unix.LOCK_UN)
		result = errors.Join(result, db.lock.Close())
	}
	return result
}
