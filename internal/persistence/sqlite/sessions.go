package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
)

type SessionStore struct {
	db       *DB
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time
}

func NewSessionStore(db *DB, idle, absolute time.Duration) *SessionStore {
	return &SessionStore{db: db, idle: idle, absolute: absolute, now: func() time.Time { return time.Now().UTC() }}
}

func tokenDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *SessionStore) Find(token string) ([]byte, bool, error) {
	now := s.now()
	var data []byte
	var id string
	var absolute int64
	err := s.db.Write.QueryRow(`SELECT s.id,s.data,s.absolute_expires_at FROM admin_sessions s
        JOIN administrators a ON a.id=s.administrator_id AND a.password_version=s.password_version
        WHERE s.token_digest=? AND s.revoked_at IS NULL AND s.idle_expires_at>? AND s.absolute_expires_at>?`,
		tokenDigest(token), millis(now), millis(now)).Scan(&id, &data, &absolute)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	idleExpiry := now.Add(s.idle)
	if idleExpiry.After(fromMillis(absolute)) {
		idleExpiry = fromMillis(absolute)
	}
	_, err = s.db.Write.Exec(`UPDATE admin_sessions SET last_seen_at=?,idle_expires_at=? WHERE id=?`, millis(now), millis(idleExpiry), id)
	return data, err == nil, err
}

func (s *SessionStore) Commit(token string, data []byte, expiry time.Time) error {
	now := s.now()
	var adminID string
	var passwordVersion int64
	if err := s.db.Write.QueryRow(`SELECT id,password_version FROM administrators LIMIT 1`).Scan(&adminID, &passwordVersion); err != nil {
		return err
	}
	absolute := now.Add(s.absolute)
	if expiry.Before(absolute) {
		absolute = expiry
	}
	idle := now.Add(s.idle)
	if idle.After(absolute) {
		idle = absolute
	}
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	_, err = s.db.Write.Exec(`INSERT INTO admin_sessions
        (id,administrator_id,token_digest,password_version,data,created_at,last_seen_at,idle_expires_at,absolute_expires_at)
        VALUES (?,?,?,?,?,?,?,?,?)
        ON CONFLICT(token_digest) DO UPDATE SET data=excluded.data,last_seen_at=excluded.last_seen_at,
        idle_expires_at=excluded.idle_expires_at,absolute_expires_at=excluded.absolute_expires_at,revoked_at=NULL`,
		id.String(), adminID, tokenDigest(token), passwordVersion, data, millis(now), millis(now), millis(idle), millis(absolute))
	return err
}

func (s *SessionStore) Delete(token string) error {
	_, err := s.db.Write.Exec(`UPDATE admin_sessions SET revoked_at=? WHERE token_digest=? AND revoked_at IS NULL`, millis(s.now()), tokenDigest(token))
	return err
}

func (s *SessionStore) RevokeAll(administratorID domain.ID) error {
	_, err := s.db.Write.Exec(`UPDATE admin_sessions SET revoked_at=? WHERE administrator_id=? AND revoked_at IS NULL`, millis(s.now()), administratorID.String())
	return err
}
