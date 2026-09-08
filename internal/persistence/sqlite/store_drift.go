package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// EnqueueDriftRemoval 为漂移目标写入移除意图；同一归属+身份只允许一条未完成记录（重放幂等）。
// 新意图在同一事务内显式取代此前该身份的 permanent_failed 记录（superseded_by 因果链），
// 旧失败是否被覆盖不再依赖 created_at 的时间比较（T155）。
//
// templateID 可以为空：面板命名空间内的孤立入站不归属任何模板，即使一个模板都没有、
// 或全部模板都已归档，也必须能被清理（FR-031）。归组键因此用 COALESCE(template_id,”)。
func (s *Store) EnqueueDriftRemoval(ctx context.Context, templateID domain.ID, inboundTag, kind, statisticsID string, now time.Time) (bool, error) {
	id, err := domain.NewID()
	if err != nil {
		return false, err
	}
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	owner := nullString(templateID.String())
	result, err := tx.ExecContext(ctx, `INSERT INTO drift_removals(id,template_id,inbound_tag,kind,statistics_id,state,attempt_count,next_attempt_at,created_at)
        VALUES (?,?,?,?,?,'pending',0,?,?)
        ON CONFLICT(COALESCE(template_id,''),statistics_id) WHERE state IN ('pending','leased','retry_wait') DO NOTHING`,
		id.String(), owner, inboundTag, kind, statisticsID, millis(now), millis(now))
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drift_removals SET superseded_by=? WHERE COALESCE(template_id,'')=? AND statistics_id=?
        AND state='permanent_failed' AND superseded_by IS NULL AND id<>?`, id.String(), templateID.String(), statisticsID, id.String()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// LeaseDueDriftRemoval 领取一条到期的移除意图（含租约过期回收）。
func (s *Store) LeaseDueDriftRemoval(ctx context.Context, owner string, now time.Time, lease time.Duration) (*ports.DriftRemoval, error) {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var removal ports.DriftRemoval
	var id, previousState string
	var next int64
	var previousOwner sql.NullString
	// 孤立入站（kind='inbound'）不归属模板，因此用 LEFT JOIN，且模板兼容性只约束身份类意图。
	// 顺带查出该入站归属用户的统计身份：清理未知客户端时，适配器要靠它判断
	// 「移除之后是否还留有这条入站真正的受管客户端」，不能从标签推断（T097）。孤立入站没有归属，留空。
	var templateID, expectedIdentity sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT d.id,d.template_id,d.inbound_tag,d.kind,d.statistics_id,d.state,d.attempt_count,
            d.next_attempt_at,d.lease_owner,i.statistics_id
        FROM drift_removals d
        LEFT JOIN inbound_templates t ON t.id=d.template_id
        LEFT JOIN dedicated_inbounds di ON di.inbound_tag=d.inbound_tag AND di.released_at IS NULL
        LEFT JOIN access_allocations a ON a.id=di.allocation_id
        LEFT JOIN xray_user_identities i ON i.id=a.identity_id
        WHERE ((d.state IN ('pending','retry_wait') AND d.next_attempt_at<=?) OR (d.state='leased' AND d.lease_expires_at<=?))
          AND (d.kind='inbound' OR t.compatibility_state <> 'incompatible')
        ORDER BY d.next_attempt_at,d.created_at LIMIT 1`, millis(now), millis(now)).Scan(
		&id, &templateID, &removal.InboundTag, &removal.Kind, &removal.StatisticsID, &previousState, &removal.AttemptCount,
		&next, &previousOwner, &expectedIdentity)
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
	removal.ID, removal.TemplateID, removal.NextAttemptAt, removal.State = domain.ID(id), domain.ID(templateID.String), fromMillis(next), domain.SyncLeased
	removal.ExpectedStatisticsID = expectedIdentity.String
	removal.Reclaimed = previousState == string(domain.SyncLeased)
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

// OrphanStaleDriftRemovals 返回不归属任何模板、且尚未被取代的永久失败孤立入站移除意图，
// 供协调器重新排队——它们没有模板可挂靠，不会出现在 StaleDriftRemovals 的结果里（FR-031）。
func (s *Store) OrphanStaleDriftRemovals(ctx context.Context) ([]ports.DriftRemoval, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT id,inbound_tag,kind,statistics_id,state,attempt_count,next_attempt_at
        FROM drift_removals WHERE template_id IS NULL AND state='permanent_failed' AND superseded_by IS NULL
        ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.DriftRemoval
	for rows.Next() {
		var removal ports.DriftRemoval
		var id string
		var next int64
		if err := rows.Scan(&id, &removal.InboundTag, &removal.Kind, &removal.StatisticsID, &removal.State,
			&removal.AttemptCount, &next); err != nil {
			return nil, err
		}
		removal.ID, removal.NextAttemptAt = domain.ID(id), fromMillis(next)
		result = append(result, removal)
	}
	return result, rows.Err()
}

// StaleDriftRemovals 返回某 profile 下仍冻结契约字段的 permanent_failed 移除意图：
// 尚未被后续意图显式取代（superseded_by IS NULL）的永久失败（与 UpdateProfile 守卫的判定一致）。
func (s *Store) StaleDriftRemovals(ctx context.Context, profileID domain.ID) ([]ports.DriftRemoval, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT d.id,d.template_id,d.inbound_tag,d.kind,d.statistics_id,d.state,d.attempt_count,d.next_attempt_at
        FROM drift_removals d JOIN inbound_templates t ON t.id=d.template_id
        WHERE d.template_id=? AND d.state='permanent_failed' AND d.superseded_by IS NULL
        ORDER BY d.created_at,d.id`, profileID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.DriftRemoval
	for rows.Next() {
		var removal ports.DriftRemoval
		var id, profile string
		var next int64
		if err := rows.Scan(&id, &profile, &removal.InboundTag, &removal.Kind, &removal.StatisticsID, &removal.State, &removal.AttemptCount, &next); err != nil {
			return nil, err
		}
		removal.ID, removal.TemplateID, removal.NextAttemptAt = domain.ID(id), domain.ID(profile), fromMillis(next)
		result = append(result, removal)
	}
	return result, rows.Err()
}
