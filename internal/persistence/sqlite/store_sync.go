package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func insertOperation(ctx context.Context, tx *sql.Tx, operation domain.SynchronizationOperation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO synchronization_operations
        (id,allocation_id,desired_revision,desired_presence,desired_credential_version,reason,phase,state,
         idempotency_key,attempt_count,next_attempt_at,lease_owner,lease_expires_at,last_error_code,last_error_summary,
         created_at,started_at,completed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, operation.ID.String(),
		operation.AllocationID.String(), operation.DesiredRevision, boolInt(operation.DesiredPresence),
		nullableInt64(operation.DesiredCredentialVersion), operation.Reason, operation.Phase, operation.State,
		operation.IdempotencyKey, operation.AttemptCount, millis(operation.NextAttemptAt), nullString(operation.LeaseOwner),
		nullTime(operation.LeaseExpiresAt), nullString(operation.LastErrorCode), nullString(operation.LastErrorSummary),
		millis(operation.CreatedAt), nullTime(operation.StartedAt), nullTime(operation.CompletedAt))
	return err
}

func (s *Store) Enqueue(ctx context.Context, operation domain.SynchronizationOperation) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE synchronization_operations
        SET state='superseded',lease_owner=NULL,lease_expires_at=NULL,completed_at=?
        WHERE allocation_id=? AND desired_revision<?
          AND state IN ('pending','leased','retry_wait','permanent_failed')`,
		millis(operation.CreatedAt), operation.AllocationID.String(), operation.DesiredRevision); err != nil {
		return err
	}
	if err := insertOperation(ctx, tx, operation); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LeaseDue(ctx context.Context, owner string, now time.Time, leaseDuration time.Duration) (*ports.SyncWork, error) {
	return s.LeaseDueSync(ctx, owner, now, leaseDuration)
}

func (s *Store) ConfirmIfRevisionCurrent(ctx context.Context, operationID domain.ID, revision domain.Revision, credentialVersion int64, present bool, now time.Time) (bool, error) {
	return s.ConfirmSync(ctx, operationID, revision, credentialVersion, present, now)
}

func (s *Store) Reschedule(ctx context.Context, id domain.ID, attempts int, next time.Time, code, summary string) error {
	return s.RescheduleSync(ctx, id, attempts, next, code, summary)
}

func (s *Store) Supersede(ctx context.Context, allocationID domain.ID, revision domain.Revision, now time.Time) (int64, error) {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE synchronization_operations
        SET state='superseded',lease_owner=NULL,lease_expires_at=NULL,completed_at=?
        WHERE allocation_id=? AND desired_revision<?
          AND state IN ('pending','leased','retry_wait','permanent_failed')`,
		millis(now), allocationID.String(), revision)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) RecordError(ctx context.Context, id domain.ID, code, summary string, now time.Time) error {
	return s.FailSync(ctx, id, code, summary, now)
}

func (s *Store) LeaseDueSync(ctx context.Context, owner string, now time.Time, leaseDuration time.Duration) (*ports.SyncWork, error) {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT o.id,o.allocation_id,o.desired_revision,o.desired_presence,o.desired_credential_version,
        o.reason,o.phase,o.state,o.idempotency_key,o.attempt_count,o.next_attempt_at,o.lease_owner,o.lease_expires_at,
        COALESCE(o.last_error_code,''),COALESCE(o.last_error_summary,''),o.created_at,o.started_at,o.completed_at
        FROM synchronization_operations o JOIN access_allocations a ON a.id=o.allocation_id
        JOIN access_profiles p ON p.id=a.profile_id
        WHERE ((o.state IN ('pending','retry_wait') AND o.next_attempt_at<=?) OR
               (o.state='leased' AND o.lease_expires_at<=?))
          AND p.compatibility_state IN ('compatible','unreachable')
        ORDER BY o.next_attempt_at,o.created_at LIMIT 1`, millis(now), millis(now))
	operation, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	leaseUntil := now.Add(leaseDuration)
	result, err := tx.ExecContext(ctx, `UPDATE synchronization_operations SET state='leased',lease_owner=?,lease_expires_at=?,
        started_at=COALESCE(started_at,?) WHERE id=? AND state IN ('pending','retry_wait','leased')`, owner, millis(leaseUntil),
		millis(now), operation.ID.String())
	if err != nil {
		return nil, err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	user, err := s.loadUser(ctx, `a.id=?`, operation.AllocationID.String())
	if err != nil {
		return nil, err
	}
	return &ports.SyncWork{Operation: operation, User: user.User, Allocation: user.Allocation, Identity: user.Identity,
		Profile: user.Profile, Credential: user.Credential}, nil
}

func scanOperation(row scanner) (domain.SynchronizationOperation, error) {
	var op domain.SynchronizationOperation
	var id, allocationID string
	var desiredCredential, leaseExpiry, started, completed sql.NullInt64
	var leaseOwner sql.NullString
	var nextAttempt, created int64
	var desired int
	err := row.Scan(&id, &allocationID, &op.DesiredRevision, &desired, &desiredCredential, &op.Reason, &op.Phase,
		&op.State, &op.IdempotencyKey, &op.AttemptCount, &nextAttempt, &leaseOwner, &leaseExpiry, &op.LastErrorCode,
		&op.LastErrorSummary, &created, &started, &completed)
	if err != nil {
		return op, err
	}
	op.ID, op.AllocationID, op.DesiredPresence = domain.ID(id), domain.ID(allocationID), desired != 0
	op.NextAttemptAt, op.CreatedAt = fromMillis(nextAttempt), fromMillis(created)
	setInt64(&op.DesiredCredentialVersion, desiredCredential)
	setTime(&op.LeaseExpiresAt, leaseExpiry)
	setTime(&op.StartedAt, started)
	setTime(&op.CompletedAt, completed)
	if leaseOwner.Valid {
		op.LeaseOwner = leaseOwner.String
	}
	return op, nil
}

func (s *Store) ConfirmSync(ctx context.Context, operationID domain.ID, revision domain.Revision, credentialVersion int64, present bool, now time.Time) (bool, error) {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var allocationID string
	var current domain.Revision
	if err := tx.QueryRowContext(ctx, `SELECT o.allocation_id,a.desired_revision FROM synchronization_operations o
        JOIN access_allocations a ON a.id=o.allocation_id WHERE o.id=?`, operationID.String()).Scan(&allocationID, &current); err != nil {
		return false, err
	}
	if current != revision {
		_, err := tx.ExecContext(ctx, `UPDATE synchronization_operations SET state='superseded',lease_owner=NULL,
            lease_expires_at=NULL,completed_at=? WHERE id=?`, millis(now), operationID.String())
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	projection := "absent"
	var syncedCredential any
	if present {
		projection = "present"
		syncedCredential = credentialVersion
		if _, err := tx.ExecContext(ctx, `UPDATE access_credentials SET state='active',activated_at=?
            WHERE allocation_id=? AND version=? AND state='pending'`, millis(now), allocationID, credentialVersion); err != nil {
			return false, err
		}
		// 轮换确认后立即销毁旧版本凭证密文（data-model §Credential rotation 第 4 步）。
		if _, err := tx.ExecContext(ctx, `UPDATE access_credentials SET state='destroyed',key_ciphertext=NULL,key_nonce=NULL,
            key_encryption_version=NULL,retired_at=? WHERE allocation_id=? AND version<? AND state!='destroyed'`,
			millis(now), allocationID, credentialVersion); err != nil {
			return false, err
		}
	} else {
		var lifecycle string
		if err := tx.QueryRowContext(ctx, `SELECT u.lifecycle_state FROM access_allocations a JOIN managed_users u ON u.id=a.user_id WHERE a.id=?`,
			allocationID).Scan(&lifecycle); err != nil {
			return false, err
		}
		if lifecycle == string(domain.LifecycleDeleted) {
			// 删除移除确认后销毁全部密钥（data-model §Atomic Transaction Boundaries 第 8 条）。
			if _, err := tx.ExecContext(ctx, `UPDATE access_credentials SET state='destroyed',key_ciphertext=NULL,key_nonce=NULL,
                key_encryption_version=NULL,retired_at=? WHERE allocation_id=? AND state!='destroyed'`, millis(now), allocationID); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_allocations SET projection_state=?,observed_present=?,synced_revision=?,
        synced_credential_version=?,last_sync_at=?,last_sync_error_code=NULL,last_sync_error_summary=NULL,updated_at=? WHERE id=?`,
		projection, boolInt(present), revision, syncedCredential, millis(now), millis(now), allocationID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE synchronization_operations SET state='succeeded',phase='done',lease_owner=NULL,
        lease_expires_at=NULL,last_error_code=NULL,last_error_summary=NULL,completed_at=? WHERE id=?`, millis(now), operationID.String()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AdvancePhase 持久化轮换阶段推进（remove_old → add_desired），用于重启后从正确阶段恢复。
func (s *Store) AdvancePhase(ctx context.Context, id domain.ID, phase domain.SyncPhase, now time.Time) error {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE synchronization_operations SET phase=?,lease_expires_at=? WHERE id=? AND state='leased'`,
		phase, millis(now.Add(30*time.Second)), id.String())
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.InvalidStateError{Message: "operation is no longer leased"}
	}
	return nil
}

func (s *Store) RescheduleSync(ctx context.Context, id domain.ID, attempts int, next time.Time, code, summary string) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var allocationID string
	if err := tx.QueryRowContext(ctx, `SELECT allocation_id FROM synchronization_operations WHERE id=?`, id.String()).Scan(&allocationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE synchronization_operations SET state='retry_wait',attempt_count=?,next_attempt_at=?,
        lease_owner=NULL,lease_expires_at=NULL,last_error_code=?,last_error_summary=? WHERE id=?`, attempts, millis(next), code, summary, id.String()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_allocations SET projection_state='pending',last_sync_error_code=?,
        last_sync_error_summary=?,updated_at=? WHERE id=?`, code, summary, millis(next), allocationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailSync(ctx context.Context, id domain.ID, code, summary string, now time.Time) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var allocationID string
	if err := tx.QueryRowContext(ctx, `SELECT allocation_id FROM synchronization_operations WHERE id=?`, id.String()).Scan(&allocationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE synchronization_operations SET state='permanent_failed',lease_owner=NULL,
        lease_expires_at=NULL,last_error_code=?,last_error_summary=?,completed_at=? WHERE id=?`, code, summary, millis(now), id.String()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_allocations SET projection_state='error',last_sync_error_code=?,
        last_sync_error_summary=?,updated_at=? WHERE id=?`, code, summary, millis(now), allocationID); err != nil {
		return err
	}
	return tx.Commit()
}
