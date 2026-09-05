package sqlite

import (
	"context"
	"database/sql"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// AuditEvents 按目标/动作/结果筛选并以 (occurred_at, id) 游标倒序分页；表为 append-only，本 Store 不提供更新或删除。
func (s *Store) AuditEvents(ctx context.Context, filter ports.AuditFilter) ([]domain.AuditEvent, *ports.AuditCursor, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id,occurred_at,actor_type,actor_id,target_type,target_id,action,result,command_id,operation_id,COALESCE(safe_summary,'')
        FROM audit_events WHERE (?='' OR target_id=?) AND (?='' OR action=?) AND (?='' OR result=?)`
	args := []any{filter.TargetID.String(), filter.TargetID.String(), filter.Action, filter.Action, filter.Result, filter.Result}
	if filter.Before != nil {
		query += ` AND (occurred_at<? OR (occurred_at=? AND id<?))`
		args = append(args, millis(filter.Before.OccurredAt), millis(filter.Before.OccurredAt), filter.Before.ID.String())
	}
	query += ` ORDER BY occurred_at DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var result []domain.AuditEvent
	for rows.Next() {
		var event domain.AuditEvent
		var id, targetID string
		var actorID, commandID, operationID sql.NullString
		var occurred int64
		if err := rows.Scan(&id, &occurred, &event.ActorType, &actorID, &event.TargetType, &targetID, &event.Action, &event.Result,
			&commandID, &operationID, &event.SafeSummary); err != nil {
			return nil, nil, err
		}
		event.ID, event.TargetID, event.OccurredAt = domain.ID(id), domain.ID(targetID), fromMillis(occurred)
		if actorID.Valid {
			value := domain.ID(actorID.String)
			event.ActorID = &value
		}
		if commandID.Valid {
			value := domain.ID(commandID.String)
			event.CommandID = &value
		}
		if operationID.Valid {
			value := domain.ID(operationID.String)
			event.OperationID = &value
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *ports.AuditCursor
	if len(result) > limit {
		last := result[limit-1]
		next = &ports.AuditCursor{OccurredAt: last.OccurredAt, ID: last.ID}
		result = result[:limit]
	}
	return result, next, nil
}
