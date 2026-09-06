package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// EnqueueDriftRemoval 为未知身份写入移除意图；同一 profile+身份只允许一条未完成记录（重放幂等）。
func (s *Store) EnqueueDriftRemoval(ctx context.Context, profileID domain.ID, statisticsID string, now time.Time) (bool, error) {
	id, err := domain.NewID()
	if err != nil {
		return false, err
	}
	result, err := s.db.Write.ExecContext(ctx, `INSERT INTO drift_removals(id,profile_id,statistics_id,state,attempt_count,next_attempt_at,created_at)
        VALUES (?,?,?,'pending',0,?,?) ON CONFLICT(profile_id,statistics_id) WHERE state IN ('pending','leased','retry_wait') DO NOTHING`,
		id.String(), profileID.String(), statisticsID, millis(now), millis(now))
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows == 1, nil
}

// LeaseDueDriftRemoval 领取一条到期的移除意图（含租约过期回收）。
func (s *Store) LeaseDueDriftRemoval(ctx context.Context, owner string, now time.Time, lease time.Duration) (*ports.DriftRemoval, error) {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var removal ports.DriftRemoval
	var id, profileID string
	var next int64
	var previousOwner sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT d.id,d.profile_id,p.inbound_tag,d.statistics_id,d.state,d.attempt_count,d.next_attempt_at,d.lease_owner
        FROM drift_removals d JOIN access_profiles p ON p.id=d.profile_id
        WHERE ((d.state IN ('pending','retry_wait') AND d.next_attempt_at<=?) OR (d.state='leased' AND d.lease_expires_at<=?))
          AND p.compatibility_state IN ('compatible','unreachable')
        ORDER BY d.next_attempt_at,d.created_at LIMIT 1`, millis(now), millis(now)).Scan(
		&id, &profileID, &removal.InboundTag, &removal.StatisticsID, &removal.State, &removal.AttemptCount, &next, &previousOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// CAS：只领取仍到期的记录；过期租约只能从刚读到的旧 owner 手中回收。
	result, err := tx.ExecContext(ctx, `UPDATE drift_removals SET state='leased',lease_owner=?,lease_expires_at=? WHERE id=?
        AND ((state IN ('pending','retry_wait') AND next_attempt_at<=?) OR (state='leased' AND lease_expires_at<=? AND COALESCE(lease_owner,'')=?))`,
		owner, millis(now.Add(lease)), id, millis(now), millis(now), previousOwner.String)
	if err != nil {
		return nil, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	removal.ID, removal.ProfileID, removal.NextAttemptAt, removal.State = domain.ID(id), domain.ID(profileID), fromMillis(next), domain.SyncLeased
	return &removal, nil
}

// RenewDriftRemovalLease 在调用 Xray 前续租并确认仍持有租约；false 表示已被回收，worker 必须放弃。
func (s *Store) RenewDriftRemovalLease(ctx context.Context, id domain.ID, owner string, now time.Time, lease time.Duration) (bool, error) {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE drift_removals SET lease_expires_at=? WHERE id=? AND state='leased' AND lease_owner=?`,
		millis(now.Add(lease)), id.String(), owner)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows == 1, nil
}

func (s *Store) finishDriftRemoval(ctx context.Context, id domain.ID, owner string, apply func(tx *sql.Tx) error) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var leaseOwner sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT state,lease_owner FROM drift_removals WHERE id=?`, id.String()).Scan(&state, &leaseOwner); err != nil {
		return err
	}
	if state != string(domain.SyncLeased) || !leaseOwner.Valid || leaseOwner.String != owner {
		return nil
	}
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteDriftRemoval 标记移除完成并写审计（reconcile_removed_unknown）。
func (s *Store) CompleteDriftRemoval(ctx context.Context, id domain.ID, owner string, now time.Time, audit domain.AuditEvent) error {
	return s.finishDriftRemoval(ctx, id, owner, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE drift_removals SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL,last_error_code=NULL,
            last_error_summary=NULL,completed_at=? WHERE id=?`, millis(now), id.String()); err != nil {
			return err
		}
		return (&txStore{tx: tx}).AppendAudit(ctx, audit)
	})
}

// RescheduleDriftRemoval 把失败的移除排入重试。
func (s *Store) RescheduleDriftRemoval(ctx context.Context, id domain.ID, owner string, attempts int, next time.Time, code, summary string) error {
	return s.finishDriftRemoval(ctx, id, owner, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE drift_removals SET state='retry_wait',attempt_count=?,next_attempt_at=?,lease_owner=NULL,lease_expires_at=NULL,
            last_error_code=?,last_error_summary=? WHERE id=?`, attempts, millis(next), code, summary, id.String())
		return err
	})
}

// FailDriftRemoval 标记永久失败并写审计。
func (s *Store) FailDriftRemoval(ctx context.Context, id domain.ID, owner, code, summary string, now time.Time, audit domain.AuditEvent) error {
	return s.finishDriftRemoval(ctx, id, owner, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE drift_removals SET state='permanent_failed',lease_owner=NULL,lease_expires_at=NULL,
            last_error_code=?,last_error_summary=?,completed_at=? WHERE id=?`, code, summary, millis(now), id.String()); err != nil {
			return err
		}
		return (&txStore{tx: tx}).AppendAudit(ctx, audit)
	})
}
