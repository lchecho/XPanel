package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// UpdateUser 在一个事务中提交显示名称、配额策略与启用意图；配额状态与同步操作由事务内重读的最新事实决定。
// 返回 (replay, operationCreated, err)。
func (s *Store) UpdateUser(ctx context.Context, record ports.UserUpdateRecord) (bool, bool, error) {
	replay, created := false, false
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
		facts, err := readFacts(ctx, tx.tx, allocationID)
		if err != nil {
			return err
		}
		exceeded := domain.IsQuotaExceeded(facts.limit, facts.accountedUplink, facts.accountedDownlink)
		decision := domain.DecideTransition(facts.AllocationFacts, record.AdminEnabled, exceeded)
		template := record.OperationTemplate
		created, err = applyDecision(ctx, tx.tx, allocationID, facts, decision, record.AdminEnabled, &template, record.Now)
		if err != nil {
			return err
		}
		if record.QuotaAudit != nil {
			audit := *record.QuotaAudit
			write := false
			switch {
			case exceeded && facts.QuotaState == domain.QuotaWithinLimit:
				audit.Action, audit.SafeSummary, write = domain.ActionQuotaExceeded, "quota lowered to or below current usage; removal requested", true
			case !exceeded && facts.QuotaState == domain.QuotaExceeded:
				audit.Action, audit.SafeSummary, write = domain.ActionCycleRestored, "quota raised above current usage; access restore requested", true
			}
			if write {
				if created {
					operationID := template.ID
					audit.OperationID = &operationID
				}
				if err := tx.AppendAudit(ctx, audit); err != nil {
					return err
				}
			}
		}
		if err := tx.SaveCommand(ctx, record.Command); err != nil {
			return err
		}
		for _, audit := range record.Audits {
			if created {
				operationID := template.ID
				audit.OperationID = &operationID
			}
			if err := tx.AppendAudit(ctx, audit); err != nil {
				return err
			}
		}
		return nil
	})
	return replay, created, err
}

// ResetCycleTraffic 清零当前周期 accounted 值并记录重置事件；恢复操作由事务内最新事实决定。
func (s *Store) ResetCycleTraffic(ctx context.Context, record ports.QuotaResetRecord) (bool, bool, error) {
	replay, created := false, false
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
		facts, err := readFacts(ctx, tx.tx, record.AllocationID.String())
		if err != nil {
			return err
		}
		decision := domain.DecideTransition(facts.AllocationFacts, facts.AdminEnabled, false)
		template := record.OperationTemplate
		created, err = applyDecision(ctx, tx.tx, record.AllocationID.String(), facts, decision, facts.AdminEnabled, &template, record.Now)
		if err != nil {
			return err
		}
		audit := record.Audit
		if created {
			operationID := template.ID
			audit.OperationID = &operationID
		}
		return tx.AppendAudit(ctx, audit)
	})
	return replay, created, err
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

// RolloverCycle 幂等地结算到期周期：在同一写事务内重读 open 周期、最新 reset_day、面板时区与分配事实，
// 逐个边界关闭旧周期并打开新周期；是否恢复访问由事务内最新事实决定。返回 (跨越的边界数, restored, err)。
func (s *Store) RolloverCycle(ctx context.Context, rollover ports.CycleRollover) (int, bool, error) {
	rolled, restored := 0, false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		template := rollover.OperationTemplate
		var err error
		rolled, restored, err = settleCycles(ctx, tx.tx, rollover.AllocationID.String(), rollover.Now, &template, rollover.Audit)
		return err
	})
	return rolled, restored, err
}

// settleCycles 在事务内结算 allocation 所有 ends_at<=now 的 open 周期（停机跨多个边界时按顺序补做），
// 新周期边界按事务内读到的最新 reset_day 与面板时区计算；跨过边界后按最新事实决定是否创建恢复操作。
// template/audit 为 nil 时由本函数生成（采集提交路径）。
func settleCycles(ctx context.Context, tx *sql.Tx, allocationID string, now time.Time, template *domain.SynchronizationOperation,
	audit *domain.AuditEvent) (int, bool, error) {
	rolled := 0
	for {
		var cycleID string
		var ends int64
		if err := tx.QueryRowContext(ctx, `SELECT id,ends_at_utc FROM quota_cycles WHERE allocation_id=? AND status='open'`, allocationID).Scan(&cycleID, &ends); err != nil {
			return rolled, false, fmt.Errorf("read open cycle: %w", err)
		}
		start := fromMillis(ends)
		if start.After(now) {
			break
		}
		var resetDay int
		var timezone string
		if err := tx.QueryRowContext(ctx, `SELECT qp.reset_day,s.quota_timezone FROM quota_policies qp JOIN panel_settings s ON s.id=1 WHERE qp.allocation_id=?`,
			allocationID).Scan(&resetDay, &timezone); err != nil {
			return rolled, false, fmt.Errorf("read quota policy: %w", err)
		}
		location, err := time.LoadLocation(timezone)
		if err != nil {
			location = time.UTC
		}
		_, end, err := domain.CycleBounds(start, location, resetDay)
		if err != nil {
			return rolled, false, err
		}
		if !end.After(start) {
			return rolled, false, fmt.Errorf("computed cycle end %s is not after %s", end, start)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET status='closed',closed_at=? WHERE id=? AND status='open'`, millis(now), cycleID); err != nil {
			return rolled, false, err
		}
		id, err := domain.NewID()
		if err != nil {
			return rolled, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO quota_cycles
            (id,allocation_id,starts_at_utc,ends_at_utc,timezone_name,reset_day,status,gross_uplink_bytes,gross_downlink_bytes,
             accounted_uplink_bytes,accounted_downlink_bytes,manual_reset_count,opened_at,closed_at)
            VALUES (?,?,?,?,?,?,'open',0,0,0,0,0,?,NULL)`, id.String(), allocationID, millis(start), millis(end), timezone, resetDay, millis(now)); err != nil {
			return rolled, false, fmt.Errorf("open cycle: %w", err)
		}
		rolled++
	}
	if rolled == 0 {
		return 0, false, nil
	}
	facts, err := readFacts(ctx, tx, allocationID)
	if err != nil {
		return rolled, false, err
	}
	decision := domain.DecideTransition(facts.AllocationFacts, facts.AdminEnabled, false)
	if template == nil {
		id, err := domain.NewID()
		if err != nil {
			return rolled, false, err
		}
		template = &domain.SynchronizationOperation{ID: id, CreatedAt: now}
	}
	restored, err := applyDecision(ctx, tx, allocationID, facts, decision, facts.AdminEnabled, template, now)
	if err != nil || !restored {
		return rolled, restored, err
	}
	if audit == nil {
		id, err := domain.NewID()
		if err != nil {
			return rolled, restored, err
		}
		var userID string
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM access_allocations WHERE id=?`, allocationID).Scan(&userID); err != nil {
			return rolled, restored, err
		}
		audit = &domain.AuditEvent{ID: id, OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "user", TargetID: domain.ID(userID),
			Action: domain.ActionCycleRestored, Result: domain.AuditAccepted, SafeSummary: "new quota cycle opened; access restore requested"}
	}
	event := *audit
	operationID := template.ID
	event.OperationID = &operationID
	return rolled, restored, (&txStore{tx: tx}).AppendAudit(ctx, event)
}
