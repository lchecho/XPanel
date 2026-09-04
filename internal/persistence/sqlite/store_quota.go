package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// UpdateUser 在一个事务中提交显示名称、配额策略、启用意图、配额状态与可选的同步操作。
func (s *Store) UpdateUser(ctx context.Context, record ports.UserUpdateRecord) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, record.Command)
		if err != nil || replay {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE managed_users SET display_name=?,normalized_name=?,revision=revision+1,updated_at=?
            WHERE id=? AND revision=? AND deleted_at IS NULL`, record.DisplayName, record.NormalizedName, millis(record.Now),
			record.UserID.String(), record.ExpectedRevision)
		if err != nil {
			return translateConstraint(err, "user name already exists")
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return &domain.ConflictError{Message: "user changed since the page was loaded"}
		}
		var allocationID string
		if err := tx.tx.QueryRowContext(ctx, `SELECT id FROM access_allocations WHERE user_id=?`, record.UserID.String()).Scan(&allocationID); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE quota_policies SET limit_bytes=?,reset_day=?,revision=revision+1,updated_at=? WHERE allocation_id=?`,
			nullableInt64(record.LimitBytes), record.ResetDay, millis(record.Now), allocationID); err != nil {
			return err
		}
		if record.Operation != nil {
			op := record.Operation
			if err := supersedeOlder(ctx, tx.tx, op.AllocationID, op.DesiredRevision, record.Now); err != nil {
				return err
			}
			if err := insertOperation(ctx, tx.tx, *op); err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET admin_enabled=?,quota_state=?,desired_revision=?,projection_state='pending',updated_at=? WHERE id=?`,
				boolInt(record.AdminEnabled), record.QuotaState, op.DesiredRevision, millis(record.Now), allocationID); err != nil {
				return err
			}
		} else if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET admin_enabled=?,quota_state=?,updated_at=? WHERE id=?`,
			boolInt(record.AdminEnabled), record.QuotaState, millis(record.Now), allocationID); err != nil {
			return err
		}
		if err := tx.SaveCommand(ctx, record.Command); err != nil {
			return err
		}
		for _, audit := range record.Audits {
			if err := tx.AppendAudit(ctx, audit); err != nil {
				return err
			}
		}
		return nil
	})
	return replay, err
}

// ResetCycleTraffic 清零当前周期 accounted 值并记录重置事件；gross、lifetime、daily 与周期边界保持不变。
func (s *Store) ResetCycleTraffic(ctx context.Context, record ports.QuotaResetRecord) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, record.Command)
		if err != nil || replay {
			return err
		}
		var previousUplink, previousDownlink int64
		if err := tx.tx.QueryRowContext(ctx, `SELECT accounted_uplink_bytes,accounted_downlink_bytes FROM quota_cycles WHERE id=? AND status='open'`,
			record.CycleID.String()).Scan(&previousUplink, &previousDownlink); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &domain.ConflictError{Message: "quota cycle is no longer open"}
			}
			return err
		}
		var uplinkCursor, downlinkCursor sql.NullInt64
		if err := tx.tx.QueryRowContext(ctx, `SELECT uplink_counter,downlink_counter FROM traffic_cursors WHERE allocation_id=?`,
			record.AllocationID.String()).Scan(&uplinkCursor, &downlinkCursor); err != nil {
			return err
		}
		if err := tx.SaveCommand(ctx, record.Command); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO quota_reset_events(id,allocation_id,quota_cycle_id,command_id,previous_accounted_uplink,
            previous_accounted_downlink,uplink_cursor,downlink_cursor,actor_id,occurred_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			record.EventID.String(), record.AllocationID.String(), record.CycleID.String(), record.Command.ID.String(), previousUplink, previousDownlink,
			uplinkCursor, downlinkCursor, record.ActorID.String(), millis(record.Now)); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE quota_cycles SET accounted_uplink_bytes=0,accounted_downlink_bytes=0,manual_reset_count=manual_reset_count+1 WHERE id=?`,
			record.CycleID.String()); err != nil {
			return err
		}
		if record.Operation != nil {
			op := record.Operation
			if err := supersedeOlder(ctx, tx.tx, op.AllocationID, op.DesiredRevision, record.Now); err != nil {
				return err
			}
			if err := insertOperation(ctx, tx.tx, *op); err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET quota_state='within_limit',desired_revision=?,projection_state='pending',updated_at=? WHERE id=?`,
				op.DesiredRevision, millis(record.Now), record.AllocationID.String()); err != nil {
				return err
			}
		} else if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET quota_state='within_limit',updated_at=? WHERE id=?`,
			millis(record.Now), record.AllocationID.String()); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, record.Audit)
	})
	return replay, err
}

// DueCycles 返回结束时间已到的 open 周期所属用户（含已删除用户，删除后周期仍需结算但不恢复）。
func (s *Store) DueCycles(ctx context.Context, now time.Time) ([]ports.UserRecord, error) {
	rows, err := s.db.Read.QueryContext(ctx, userSelect+` WHERE qc.ends_at_utc<=? ORDER BY qc.ends_at_utc,u.id`, millis(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.UserRecord
	for rows.Next() {
		record, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// NextCycleEnd 返回最近一个 open 周期的结束时刻，供 scheduler 设置边界定时器。
func (s *Store) NextCycleEnd(ctx context.Context) (*time.Time, error) {
	var value sql.NullInt64
	if err := s.db.Read.QueryRowContext(ctx, `SELECT MIN(ends_at_utc) FROM quota_cycles WHERE status='open'`).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, nil
	}
	result := fromMillis(value.Int64)
	return &result, nil
}

// RolloverCycle 幂等地关闭旧周期并打开新周期；重复执行不会重复结算或重复恢复。
func (s *Store) RolloverCycle(ctx context.Context, rollover ports.CycleRollover) error {
	return s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		result, err := tx.tx.ExecContext(ctx, `UPDATE quota_cycles SET status='closed',closed_at=? WHERE id=? AND status='open'`,
			millis(rollover.Now), rollover.OldCycleID.String())
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return nil // 已由更早的一次切换处理。
		}
		c := rollover.NewCycle
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO quota_cycles
            (id,allocation_id,starts_at_utc,ends_at_utc,timezone_name,reset_day,status,gross_uplink_bytes,gross_downlink_bytes,
             accounted_uplink_bytes,accounted_downlink_bytes,manual_reset_count,opened_at,closed_at)
            VALUES (?,?,?,?,?,?,'open',0,0,0,0,0,?,NULL) ON CONFLICT(allocation_id,starts_at_utc) DO NOTHING`,
			c.ID.String(), c.AllocationID.String(), millis(c.StartsAt), millis(c.EndsAt), c.Timezone, c.ResetDay, millis(rollover.Now)); err != nil {
			return err
		}
		if rollover.Operation != nil {
			op := rollover.Operation
			if err := supersedeOlder(ctx, tx.tx, op.AllocationID, op.DesiredRevision, rollover.Now); err != nil {
				return err
			}
			if err := insertOperation(ctx, tx.tx, *op); err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET quota_state='within_limit',desired_revision=?,projection_state='pending',updated_at=? WHERE id=?`,
				op.DesiredRevision, millis(rollover.Now), rollover.AllocationID.String()); err != nil {
				return err
			}
		} else if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET quota_state='within_limit',updated_at=? WHERE id=?`,
			millis(rollover.Now), rollover.AllocationID.String()); err != nil {
			return err
		}
		if rollover.Audit != nil {
			return tx.AppendAudit(ctx, *rollover.Audit)
		}
		return nil
	})
}
