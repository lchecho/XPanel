package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
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

// record 计算一条会话行的时间边界（idle/absolute 取较早者）。
func (s *SessionStore) record(token string, data []byte, expiry time.Time) (ports.SessionRecord, error) {
	now := s.now()
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
		return ports.SessionRecord{}, err
	}
	return ports.SessionRecord{ID: id, TokenDigest: tokenDigest(token), Data: data, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: idle, AbsoluteExpiresAt: absolute}, nil
}

// Commit 持久化会话数据（辅助刷新路径）；已撤销的令牌不会因迟到的提交而恢复有效（登出后并发请求不得复活会话）。
func (s *SessionStore) Commit(token string, data []byte, expiry time.Time) error {
	var adminID string
	var passwordVersion int64
	if err := s.db.Write.QueryRow(`SELECT id,password_version FROM administrators LIMIT 1`).Scan(&adminID, &passwordVersion); err != nil {
		return err
	}
	record, err := s.record(token, data, expiry)
	if err != nil {
		return err
	}
	_, err = s.db.Write.Exec(`INSERT INTO admin_sessions
        (id,administrator_id,token_digest,password_version,data,created_at,last_seen_at,idle_expires_at,absolute_expires_at)
        VALUES (?,?,?,?,?,?,?,?,?)
        ON CONFLICT(token_digest) DO UPDATE SET data=excluded.data,last_seen_at=excluded.last_seen_at,
        idle_expires_at=excluded.idle_expires_at,absolute_expires_at=excluded.absolute_expires_at
        WHERE admin_sessions.revoked_at IS NULL`,
		record.ID.String(), adminID, record.TokenDigest, passwordVersion, record.Data, millis(record.CreatedAt), millis(record.LastSeenAt),
		millis(record.IdleExpiresAt), millis(record.AbsoluteExpiresAt))
	return err
}

// ErrSessionNotLive 表示事务内撤销时会话已不存在或已撤销：登出不能宣称成功。
var ErrSessionNotLive = errors.New("session is not live")

// CommitCtx 是安全关键路径：ctx 内携带写事务时（登录），会话行与登录审计在同一事务提交；否则等同 Commit。
func (s *SessionStore) CommitCtx(ctx context.Context, token string, data []byte, expiry time.Time) error {
	if tx, ok := ports.WriteTxFromContext(ctx); ok {
		record, err := s.record(token, data, expiry)
		if err != nil {
			return err
		}
		return tx.InsertSession(ctx, record)
	}
	return s.Commit(token, data, expiry)
}

// DeleteCtx：ctx 内携带写事务时（登出）在同一事务内撤销当前会话，未撤销任何行即失败；否则等同 Delete。
func (s *SessionStore) DeleteCtx(ctx context.Context, token string) error {
	if tx, ok := ports.WriteTxFromContext(ctx); ok {
		revoked, err := tx.RevokeSession(ctx, tokenDigest(token), s.now())
		if err != nil {
			return err
		}
		if !revoked {
			return ErrSessionNotLive
		}
		return nil
	}
	return s.Delete(token)
}

func (s *SessionStore) FindCtx(_ context.Context, token string) ([]byte, bool, error) {
	return s.Find(token)
}

func (s *SessionStore) Delete(token string) error {
	_, err := s.db.Write.Exec(`UPDATE admin_sessions SET revoked_at=? WHERE token_digest=? AND revoked_at IS NULL`, millis(s.now()), tokenDigest(token))
	return err
}

func (s *SessionStore) RevokeAll(administratorID domain.ID) error {
	_, err := s.db.Write.Exec(`UPDATE admin_sessions SET revoked_at=? WHERE administrator_id=? AND revoked_at IS NULL`, millis(s.now()), administratorID.String())
	return err
}
