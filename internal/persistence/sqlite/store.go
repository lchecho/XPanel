package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

type Store struct{ db *DB }

func NewStore(db *DB) *Store  { return &Store{db: db} }
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *DB      { return s.db }

type txStore struct{ tx *sql.Tx }

func (s *Store) WithWriteTx(ctx context.Context, fn func(ports.WriteTx) error) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	wrapped := &txStore{tx: tx}
	if err := fn(wrapped); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) WithReadTx(ctx context.Context, fn func(ports.ReadTx) error) error {
	tx, err := s.db.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	wrapped := &txStore{tx: tx}
	if err := fn(wrapped); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func millis(value time.Time) int64     { return value.UTC().UnixMilli() }
func fromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }

func nullID(value *domain.ID) any {
	if value == nil {
		return nil
	}
	return value.String()
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return millis(*value)
}

func (t *txStore) FindCommand(ctx context.Context, id domain.ID) (*domain.DomainCommand, error) {
	row := t.tx.QueryRowContext(ctx, `SELECT actor_type, actor_id, command_type, target_type, target_id,
        request_fingerprint, state, COALESCE(result_reference,''), created_at, completed_at
        FROM domain_commands WHERE id=?`, id.String())
	var command domain.DomainCommand
	var actorID sql.NullString
	var targetID string
	var created int64
	var completed sql.NullInt64
	if err := row.Scan(&command.ActorType, &actorID, &command.CommandType, &command.TargetType, &targetID,
		&command.RequestFingerprint, &command.State, &command.ResultReference, &created, &completed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	command.ID = id
	command.TargetID = domain.ID(targetID)
	command.CreatedAt = fromMillis(created)
	if actorID.Valid {
		value := domain.ID(actorID.String)
		command.ActorID = &value
	}
	if completed.Valid {
		value := fromMillis(completed.Int64)
		command.CompletedAt = &value
	}
	return &command, nil
}

func (t *txStore) SaveCommand(ctx context.Context, command domain.DomainCommand) error {
	existing, err := t.FindCommand(ctx, command.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		if !bytes.Equal(existing.RequestFingerprint, command.RequestFingerprint) {
			return &domain.ConflictError{Message: "request identifier was reused with different input"}
		}
		return nil
	}
	_, err = t.tx.ExecContext(ctx, `INSERT INTO domain_commands
        (id,actor_type,actor_id,command_type,target_type,target_id,request_fingerprint,state,result_reference,created_at,completed_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?)`, command.ID.String(), command.ActorType, nullID(command.ActorID), command.CommandType,
		command.TargetType, command.TargetID.String(), command.RequestFingerprint, command.State, command.ResultReference,
		millis(command.CreatedAt), nullTime(command.CompletedAt))
	return err
}

func (t *txStore) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO audit_events
        (id,occurred_at,actor_type,actor_id,target_type,target_id,action,result,command_id,operation_id,safe_summary)
        VALUES (?,?,?,?,?,?,?,?,?,?,?)`, event.ID.String(), millis(event.OccurredAt), event.ActorType, nullID(event.ActorID),
		event.TargetType, event.TargetID.String(), event.Action, event.Result, nullID(event.CommandID), nullID(event.OperationID), event.SafeSummary)
	return err
}

func (t *txStore) Administrator(ctx context.Context) (*ports.AdministratorRecord, error) {
	row := t.tx.QueryRowContext(ctx, `SELECT id,username,password_hash,password_version,created_at,updated_at
        FROM administrators LIMIT 1`)
	var result ports.AdministratorRecord
	var id string
	var created, updated int64
	if err := row.Scan(&id, &result.Username, &result.PasswordHash, &result.PasswordVersion, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	result.ID = domain.ID(id)
	result.CreatedAt = fromMillis(created)
	result.UpdatedAt = fromMillis(updated)
	return &result, nil
}

func (t *txStore) CreateAdministrator(ctx context.Context, admin ports.AdministratorRecord) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO administrators
        (id,username,password_hash,password_version,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		admin.ID.String(), admin.Username, admin.PasswordHash, admin.PasswordVersion, millis(admin.CreatedAt), millis(admin.UpdatedAt))
	return err
}

func (t *txStore) UpdateAdministratorPassword(ctx context.Context, id domain.ID, hash string, version int64, now time.Time) error {
	result, err := t.tx.ExecContext(ctx, `UPDATE administrators SET password_hash=?,password_version=?,updated_at=? WHERE id=?`,
		hash, version, millis(now), id.String())
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.NotFoundError{Resource: "administrator"}
	}
	return nil
}

func (t *txStore) RevokeAllSessions(ctx context.Context, id domain.ID, now time.Time) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE admin_sessions SET revoked_at=? WHERE administrator_id=? AND revoked_at IS NULL`, millis(now), id.String())
	return err
}

func (t *txStore) PanelSettings(ctx context.Context) (ports.PanelSettingsRecord, error) {
	row := t.tx.QueryRowContext(ctx, `SELECT quota_timezone,key_verifier,key_verifier_nonce,key_encryption_version,revision,created_at,updated_at
        FROM panel_settings WHERE id=1`)
	var result ports.PanelSettingsRecord
	var created, updated int64
	if err := row.Scan(&result.QuotaTimezone, &result.KeyVerifier, &result.KeyVerifierNonce, &result.KeyEncryptionVersion,
		&result.Revision, &created, &updated); err != nil {
		return result, err
	}
	result.CreatedAt = fromMillis(created)
	result.UpdatedAt = fromMillis(updated)
	return result, nil
}

func (s *Store) EnsureSingletons(ctx context.Context, timezone, endpoint, version string, verifier, nonce []byte, now time.Time) error {
	return s.WithWriteTx(ctx, func(tx ports.WriteTx) error {
		wrapped := tx.(*txStore)
		if _, err := wrapped.tx.ExecContext(ctx, `INSERT INTO panel_settings
            (id,quota_timezone,key_verifier,key_verifier_nonce,key_encryption_version,revision,created_at,updated_at)
            VALUES (1,?,?,?,?,0,?,?) ON CONFLICT(id) DO NOTHING`, timezone, verifier, nonce, 1, millis(now), millis(now)); err != nil {
			return err
		}
		var id string
		err := wrapped.tx.QueryRowContext(ctx, `SELECT id FROM managed_xray_instances WHERE singleton=1`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			newID, idErr := domain.NewID()
			if idErr != nil {
				return idErr
			}
			_, err = wrapped.tx.ExecContext(ctx, `INSERT INTO managed_xray_instances
                (id,singleton,name,api_endpoint,supported_runtime_version,health_state,updated_at)
                VALUES (?,1,'local-xray',?,?,'unknown',?)`, newID.String(), endpoint, version, millis(now))
			return err
		}
		if err != nil {
			return err
		}
		_, err = wrapped.tx.ExecContext(ctx, `UPDATE managed_xray_instances SET api_endpoint=?,supported_runtime_version=?,updated_at=? WHERE id=?`,
			endpoint, version, millis(now), id)
		return err
	})
}

func (s *Store) VerifyKey(ctx context.Context, verify func([]byte, []byte) error) error {
	var settings ports.PanelSettingsRecord
	err := s.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var inner error
		settings, inner = tx.PanelSettings(ctx)
		return inner
	})
	if err != nil {
		return err
	}
	if err := verify(settings.KeyVerifier, settings.KeyVerifierNonce); err != nil {
		return fmt.Errorf("root key verifier: %w", err)
	}
	return nil
}

func (s *Store) Settings(ctx context.Context) (ports.PanelSettingsRecord, error) {
	var settings ports.PanelSettingsRecord
	err := s.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var err error
		settings, err = tx.PanelSettings(ctx)
		return err
	})
	return settings, err
}

// UpdateSettings 按 revision 更新全局配额时区；新值只影响未来周期（data-model §PanelSettings）。
func (s *Store) UpdateSettings(ctx context.Context, timezone string, expected domain.Revision, command domain.DomainCommand, audit domain.AuditEvent) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, command)
		if err != nil || replay {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE panel_settings SET quota_timezone=?,revision=revision+1,updated_at=? WHERE id=1 AND revision=?`,
			timezone, millis(audit.OccurredAt), expected)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return &domain.ConflictError{Message: "settings changed since the page was loaded"}
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
	return replay, err
}

func (s *Store) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	return s.WithWriteTx(ctx, func(tx ports.WriteTx) error { return tx.AppendAudit(ctx, event) })
}
