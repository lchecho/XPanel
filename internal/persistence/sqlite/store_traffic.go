package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// CollectionTargets 返回当前应读取计数的分配：未删除且 Xray 投影为 present 的分配（含超限但尚未移除者）。
func (s *Store) CollectionTargets(ctx context.Context) ([]ports.CollectionTarget, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT u.id,u.display_name,u.normalized_name,u.lifecycle_state,u.revision,u.created_at,u.updated_at,u.deleted_at,
        i.id,i.instance_id,i.template_id,i.statistics_id,i.kind,i.created_at,
        a.id,a.user_id,a.template_id,a.identity_id,a.admin_enabled,a.quota_state,a.projection_state,a.observed_present,
        a.desired_revision,a.synced_revision,a.desired_credential_version,a.synced_credential_version,a.last_sync_at,
        COALESCE(a.last_sync_error_code,''),COALESCE(a.last_sync_error_summary,''),a.created_at,a.updated_at,
        d.inbound_tag,
        qp.allocation_id,qp.limit_bytes,qp.reset_day,qp.revision,qp.created_at,qp.updated_at,
        qc.id,qc.allocation_id,qc.starts_at_utc,qc.ends_at_utc,qc.timezone_name,qc.reset_day,qc.status,
        qc.gross_uplink_bytes,qc.gross_downlink_bytes,qc.accounted_uplink_bytes,qc.accounted_downlink_bytes,qc.manual_reset_count,qc.opened_at,qc.closed_at,
        tc.boot_epoch,tc.uplink_counter,tc.downlink_counter,tc.uplink_epoch,tc.downlink_epoch,tc.last_observed_at,tc.last_success_at,tc.missing_since,
        tt.uplink_bytes,tt.downlink_bytes
        FROM managed_users u
        JOIN access_allocations a ON a.user_id=u.id
        JOIN xray_user_identities i ON i.id=a.identity_id
        JOIN dedicated_inbounds d ON d.allocation_id=a.id AND d.released_at IS NULL
        JOIN quota_policies qp ON qp.allocation_id=a.id
        JOIN quota_cycles qc ON qc.allocation_id=a.id AND qc.status='open'
        JOIN traffic_cursors tc ON tc.allocation_id=a.id
        JOIN allocation_traffic_totals tt ON tt.allocation_id=a.id
        WHERE u.deleted_at IS NULL AND a.projection_state='present'
        ORDER BY u.normalized_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.CollectionTarget
	for rows.Next() {
		var t ports.CollectionTarget
		var userID, identityID, identityInstance, identityProfile, allocationID, allocationUser, allocationProfile, allocationIdentity string
		var policyAllocation, cycleID, cycleAllocation string
		var userCreated, userUpdated, identityCreated, allocationCreated, allocationUpdated, policyCreated, policyUpdated, starts, ends, opened int64
		var userDeleted, lastSync, cycleClosed, observed, syncedCredential, limit sql.NullInt64
		var bootEpoch sql.NullString
		var uplinkCounter, downlinkCounter, lastObserved, lastSuccess, missingSince sql.NullInt64
		if err := rows.Scan(&userID, &t.User.DisplayName, &t.User.NormalizedName, &t.User.Lifecycle, &t.User.Revision, &userCreated, &userUpdated, &userDeleted,
			&identityID, &identityInstance, &identityProfile, &t.Identity.StatisticsID, &t.Identity.Kind, &identityCreated,
			&allocationID, &allocationUser, &allocationProfile, &allocationIdentity, &t.Allocation.AdminEnabled, &t.Allocation.QuotaState,
			&t.Allocation.ProjectionState, &observed, &t.Allocation.DesiredRevision, &t.Allocation.SyncedRevision, &t.Allocation.DesiredCredentialVersion,
			&syncedCredential, &lastSync, &t.Allocation.LastSyncErrorCode, &t.Allocation.LastSyncErrorSummary, &allocationCreated, &allocationUpdated,
			&t.ProfileTag,
			&policyAllocation, &limit, &t.Policy.ResetDay, &t.Policy.Revision, &policyCreated, &policyUpdated,
			&cycleID, &cycleAllocation, &starts, &ends, &t.Cycle.Timezone, &t.Cycle.ResetDay, &t.Cycle.Status,
			&t.Cycle.GrossUplinkBytes, &t.Cycle.GrossDownlinkBytes, &t.Cycle.AccountedUplinkBytes, &t.Cycle.AccountedDownlinkBytes, &t.Cycle.ManualResetCount, &opened, &cycleClosed,
			&bootEpoch, &uplinkCounter, &downlinkCounter, &t.Cursor.UplinkEpoch, &t.Cursor.DownlinkEpoch, &lastObserved, &lastSuccess, &missingSince,
			&t.TotalUplink, &t.TotalDownlink); err != nil {
			return nil, err
		}
		t.User.ID, t.User.CreatedAt, t.User.UpdatedAt = domain.ID(userID), fromMillis(userCreated), fromMillis(userUpdated)
		setTime(&t.User.DeletedAt, userDeleted)
		t.Identity.ID, t.Identity.InstanceID, t.Identity.TemplateID, t.Identity.CreatedAt = domain.ID(identityID), domain.ID(identityInstance), domain.ID(identityProfile), fromMillis(identityCreated)
		t.Allocation.ID, t.Allocation.UserID, t.Allocation.TemplateID, t.Allocation.IdentityID = domain.ID(allocationID), domain.ID(allocationUser), domain.ID(allocationProfile), domain.ID(allocationIdentity)
		t.Allocation.CreatedAt, t.Allocation.UpdatedAt = fromMillis(allocationCreated), fromMillis(allocationUpdated)
		setBool(&t.Allocation.ObservedPresent, observed)
		setInt64(&t.Allocation.SyncedCredentialVersion, syncedCredential)
		setTime(&t.Allocation.LastSyncAt, lastSync)
		t.Policy.AllocationID, t.Policy.CreatedAt, t.Policy.UpdatedAt = domain.ID(policyAllocation), fromMillis(policyCreated), fromMillis(policyUpdated)
		setInt64(&t.Policy.LimitBytes, limit)
		t.Cycle.ID, t.Cycle.AllocationID = domain.ID(cycleID), domain.ID(cycleAllocation)
		t.Cycle.StartsAt, t.Cycle.EndsAt, t.Cycle.OpenedAt = fromMillis(starts), fromMillis(ends), fromMillis(opened)
		setTime(&t.Cycle.ClosedAt, cycleClosed)
		t.Cursor.AllocationID = t.Allocation.ID
		if bootEpoch.Valid {
			t.Cursor.BootEpoch = bootEpoch.String
		}
		setInt64(&t.Cursor.UplinkCounter, uplinkCounter)
		setInt64(&t.Cursor.DownlinkCounter, downlinkCounter)
		setTime(&t.Cursor.LastObservedAt, lastObserved)
		setTime(&t.Cursor.LastSuccessAt, lastSuccess)
		setTime(&t.Cursor.MissingSince, missingSince)
		result = append(result, t)
	}
	return result, rows.Err()
}

// CommitTrafficBatch 在一个短事务中写入一轮采集的全部结果：游标、累计、日聚合、周期、事件，
// 并在事务内按最新事实（策略、启用意图、生命周期、open 周期）重新判定越界，返回本轮新建的封禁操作数。
func (s *Store) CommitTrafficBatch(ctx context.Context, batch ports.TrafficBatch) (ports.TrafficCommitResult, error) {
	result := ports.TrafficCommitResult{}
	if len(batch.Updates) == 0 {
		return result, nil
	}
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, update := range batch.Updates {
		// 样本完成时间已跨过 open 周期边界时先在同一事务内结算周期，增量才进入正确的新周期（FR-016/FR-018）。
		rolled, restored, err := settleCycles(ctx, tx, update.AllocationID.String(), batch.ObservedAt, nil, nil)
		if err != nil {
			return result, err
		}
		result.Rolled += rolled
		if restored {
			result.Restored++
		}
		c := update.Cursor
		if _, err := tx.ExecContext(ctx, `UPDATE traffic_cursors SET boot_epoch=?,uplink_counter=?,downlink_counter=?,uplink_epoch=?,
            downlink_epoch=?,last_observed_at=?,last_success_at=?,missing_since=?,updated_at=? WHERE allocation_id=?`,
			nullString(c.BootEpoch), nullableInt64(c.UplinkCounter), nullableInt64(c.DownlinkCounter), c.UplinkEpoch, c.DownlinkEpoch,
			nullTime(c.LastObservedAt), nullTime(c.LastSuccessAt), nullTime(c.MissingSince), millis(batch.ObservedAt), update.AllocationID.String()); err != nil {
			return result, fmt.Errorf("update cursor: %w", err)
		}
		if update.UplinkDelta != 0 || update.DownlinkDelta != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE allocation_traffic_totals SET uplink_bytes=uplink_bytes+?,downlink_bytes=downlink_bytes+?,updated_at=? WHERE allocation_id=?`,
				update.UplinkDelta, update.DownlinkDelta, millis(batch.ObservedAt), update.AllocationID.String()); err != nil {
				return result, fmt.Errorf("update totals: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO daily_traffic_aggregates(allocation_id,day_start_utc,local_date,timezone_name,uplink_bytes,downlink_bytes,updated_at)
                VALUES (?,?,?,?,?,?,?) ON CONFLICT(allocation_id,day_start_utc) DO UPDATE SET uplink_bytes=uplink_bytes+excluded.uplink_bytes,
                downlink_bytes=downlink_bytes+excluded.downlink_bytes,updated_at=excluded.updated_at`,
				update.AllocationID.String(), millis(update.DayStartUTC), update.LocalDate, update.Timezone, update.UplinkDelta, update.DownlinkDelta, millis(batch.ObservedAt)); err != nil {
				return result, fmt.Errorf("update daily aggregate: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET gross_uplink_bytes=gross_uplink_bytes+?,gross_downlink_bytes=gross_downlink_bytes+?,
                accounted_uplink_bytes=accounted_uplink_bytes+?,accounted_downlink_bytes=accounted_downlink_bytes+? WHERE allocation_id=? AND status='open'`,
				update.UplinkDelta, update.DownlinkDelta, update.UplinkDelta, update.DownlinkDelta, update.AllocationID.String()); err != nil {
				return result, fmt.Errorf("update cycle: %w", err)
			}
		}
		for _, event := range update.Events {
			if err := insertContinuityEvent(ctx, tx, event); err != nil {
				return result, err
			}
		}
		facts, err := readFacts(ctx, tx, update.AllocationID.String())
		if err != nil {
			return result, err
		}
		exceeded := domain.IsQuotaExceeded(facts.limit, facts.accountedUplink, facts.accountedDownlink)
		if !exceeded || facts.QuotaState != domain.QuotaWithinLimit {
			continue
		}
		decision := domain.DecideTransition(facts.AllocationFacts, facts.AdminEnabled, true)
		created, err := applyDecision(ctx, tx, update.AllocationID.String(), facts, decision, facts.AdminEnabled, update.QuotaBlock, batch.ObservedAt)
		if err != nil {
			return result, err
		}
		if created {
			result.Blocked++
		}
		if update.Audit != nil {
			audit := *update.Audit
			if created {
				operationID := update.QuotaBlock.ID
				audit.OperationID = &operationID
			}
			if err := (&txStore{tx: tx}).AppendAudit(ctx, audit); err != nil {
				return result, err
			}
		}
	}
	return result, tx.Commit()
}

// allocationFacts 是事务内重读的分配事实，含当前策略与 open 周期 accounted 值。
type allocationFacts struct {
	domain.AllocationFacts
	limit             *int64
	accountedUplink   int64
	accountedDownlink int64
}

func readFacts(ctx context.Context, tx *sql.Tx, allocationID string) (allocationFacts, error) {
	var facts allocationFacts
	var limit sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT u.lifecycle_state,a.admin_enabled,a.quota_state,a.desired_revision,a.desired_credential_version,
        qp.limit_bytes,qc.accounted_uplink_bytes,qc.accounted_downlink_bytes
        FROM access_allocations a JOIN managed_users u ON u.id=a.user_id JOIN quota_policies qp ON qp.allocation_id=a.id
        JOIN quota_cycles qc ON qc.allocation_id=a.id AND qc.status='open' WHERE a.id=?`, allocationID).Scan(
		&facts.Lifecycle, &facts.AdminEnabled, &facts.QuotaState, &facts.DesiredRevision, &facts.DesiredCredentialVersion,
		&limit, &facts.accountedUplink, &facts.accountedDownlink)
	if err != nil {
		return facts, fmt.Errorf("read allocation facts: %w", err)
	}
	if limit.Valid {
		value := limit.Int64
		facts.limit = &value
	}
	return facts, nil
}

// applyDecision 写入决策结果：配额状态、启用意图，以及（需要时）以事务内 revision+1 创建的同步操作。
func applyDecision(ctx context.Context, tx *sql.Tx, allocationID string, facts allocationFacts, decision domain.TransitionDecision,
	adminEnabled bool, template *domain.SynchronizationOperation, now time.Time) (bool, error) {
	if decision.NeedsOperation() && template != nil {
		op := *template
		op.AllocationID = domain.ID(allocationID)
		op.DesiredRevision = facts.DesiredRevision + 1
		op.DesiredPresence = decision.WillBePresent
		version := facts.DesiredCredentialVersion
		op.DesiredCredentialVersion = &version
		op.Reason, op.Phase, op.State = decision.Reason, decision.Phase, domain.SyncPending
		op.IdempotencyKey = domain.SyncIdempotencyKey(op.AllocationID, op.DesiredRevision, op.Phase)
		op.AttemptCount, op.LeaseOwner, op.LeaseExpiresAt = 0, "", nil
		op.NextAttemptAt = now
		if op.CreatedAt.IsZero() {
			op.CreatedAt = now
		}
		if err := supersedeOlder(ctx, tx, op.AllocationID, op.DesiredRevision, now); err != nil {
			return false, err
		}
		if err := insertOperation(ctx, tx, op); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE access_allocations SET admin_enabled=?,quota_state=?,desired_revision=?,projection_state='pending',updated_at=? WHERE id=?`,
			boolInt(adminEnabled), decision.QuotaState, op.DesiredRevision, millis(now), allocationID)
		return err == nil, err
	}
	_, err := tx.ExecContext(ctx, `UPDATE access_allocations SET admin_enabled=?,quota_state=?,updated_at=? WHERE id=?`,
		boolInt(adminEnabled), decision.QuotaState, millis(now), allocationID)
	return false, err
}

func insertContinuityEvent(ctx context.Context, tx *sql.Tx, record ports.ContinuityEventRecord) error {
	e := record.Event
	_, err := tx.ExecContext(ctx, `INSERT INTO traffic_continuity_events(id,allocation_id,type,direction,old_counter,new_counter,counter_epoch,occurred_at,safe_summary)
        VALUES (?,?,?,?,?,?,?,?,?)`, record.ID.String(), record.AllocationID.String(), e.Type, nullString(e.Direction), nullableInt64(e.OldCounter),
		nullableInt64(e.NewCounter), e.CounterEpoch, millis(record.OccurredAt), e.Summary)
	return err
}

func supersedeOlder(ctx context.Context, tx *sql.Tx, allocationID domain.ID, revision domain.Revision, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE synchronization_operations SET state='superseded',lease_owner=NULL,lease_expires_at=NULL,completed_at=?
        WHERE allocation_id=? AND desired_revision<? AND state IN ('pending','leased','retry_wait','permanent_failed')`,
		millis(now), allocationID.String(), revision)
	return err
}
