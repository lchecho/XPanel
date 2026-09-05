package sqlite

import (
	"context"
	"database/sql"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// FailedOperations 返回最近处于重试或永久失败状态的同步操作（供仪表盘展示同步故障摘要）。
func (s *Store) FailedOperations(ctx context.Context, limit int) ([]ports.FailedOperationRecord, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT u.id,u.display_name,o.reason,o.state,COALESCE(o.last_error_code,''),COALESCE(o.last_error_summary,''),
        o.attempt_count,o.next_attempt_at,COALESCE(o.completed_at,o.started_at,o.created_at)
        FROM synchronization_operations o JOIN access_allocations a ON a.id=o.allocation_id JOIN managed_users u ON u.id=a.user_id
        WHERE o.state IN ('retry_wait','permanent_failed') OR (o.state IN ('pending','leased') AND o.last_error_code IS NOT NULL)
        ORDER BY o.next_attempt_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.FailedOperationRecord
	for rows.Next() {
		var record ports.FailedOperationRecord
		var userID string
		var next, updated int64
		if err := rows.Scan(&userID, &record.DisplayName, &record.Reason, &record.State, &record.ErrorCode, &record.ErrorSummary,
			&record.AttemptCount, &next, &updated); err != nil {
			return nil, err
		}
		record.UserID = domain.ID(userID)
		record.NextAttemptAt, record.UpdatedAt = fromMillis(next), fromMillis(updated)
		result = append(result, record)
	}
	return result, rows.Err()
}

// LastCollectionAt 返回最近一次成功读到计数的时刻（所有游标的最大 last_success_at）。
func (s *Store) LastCollectionAt(ctx context.Context) (*time.Time, error) {
	var value sql.NullInt64
	if err := s.db.Read.QueryRowContext(ctx, `SELECT MAX(last_success_at) FROM traffic_cursors`).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, nil
	}
	result := fromMillis(value.Int64)
	return &result, nil
}

// DailyAggregates 返回 [from, to) 内的日聚合，按日期升序。
func (s *Store) DailyAggregates(ctx context.Context, allocationID domain.ID, from, to time.Time) ([]ports.DailyAggregateRecord, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT day_start_utc,local_date,timezone_name,uplink_bytes,downlink_bytes FROM daily_traffic_aggregates
        WHERE allocation_id=? AND day_start_utc>=? AND day_start_utc<? ORDER BY day_start_utc`, allocationID.String(), millis(from), millis(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.DailyAggregateRecord
	for rows.Next() {
		var record ports.DailyAggregateRecord
		var day int64
		if err := rows.Scan(&day, &record.LocalDate, &record.Timezone, &record.UplinkBytes, &record.DownlinkBytes); err != nil {
			return nil, err
		}
		record.DayStartUTC = fromMillis(day)
		result = append(result, record)
	}
	return result, rows.Err()
}

// ContinuityEvents 返回最近的统计连续性事件，按时间倒序。
func (s *Store) ContinuityEvents(ctx context.Context, allocationID domain.ID, limit int) ([]ports.ContinuityEventListRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT type,COALESCE(direction,''),occurred_at,safe_summary FROM traffic_continuity_events
        WHERE allocation_id=? ORDER BY occurred_at DESC,id DESC LIMIT ?`, allocationID.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.ContinuityEventListRecord
	for rows.Next() {
		var record ports.ContinuityEventListRecord
		var occurred int64
		if err := rows.Scan(&record.Type, &record.Direction, &occurred, &record.Summary); err != nil {
			return nil, err
		}
		record.OccurredAt = fromMillis(occurred)
		result = append(result, record)
	}
	return result, rows.Err()
}

// SyncOperations 返回某分配最近的同步操作历史，按 revision 倒序。
func (s *Store) SyncOperations(ctx context.Context, allocationID domain.ID, limit int) ([]domain.SynchronizationOperation, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Read.QueryContext(ctx, `SELECT o.id,o.allocation_id,o.desired_revision,o.desired_presence,o.desired_credential_version,
        o.reason,o.phase,o.state,o.idempotency_key,o.attempt_count,o.next_attempt_at,o.lease_owner,o.lease_expires_at,
        COALESCE(o.last_error_code,''),COALESCE(o.last_error_summary,''),o.created_at,o.started_at,o.completed_at
        FROM synchronization_operations o WHERE o.allocation_id=? ORDER BY o.desired_revision DESC,o.created_at DESC LIMIT ?`, allocationID.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.SynchronizationOperation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, op)
	}
	return result, rows.Err()
}
