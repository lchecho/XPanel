package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func (s *Store) CreateUser(ctx context.Context, record ports.UserCreateRecord) (domain.ID, bool, error) {
	var replay bool
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		existing, err := tx.FindCommand(ctx, record.Command.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !bytes.Equal(existing.RequestFingerprint, record.Command.RequestFingerprint) {
				return &domain.ConflictError{Message: "request identifier was reused with different input"}
			}
			replay = true
			record.User.ID = existing.TargetID
			return nil
		}
		u, i, a, c := record.User, record.Identity, record.Allocation, record.Credential
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO managed_users
            (id,display_name,normalized_name,lifecycle_state,revision,created_at,updated_at,deleted_at)
            VALUES (?,?,?,?,?,?,?,?)`, u.ID.String(), u.DisplayName, u.NormalizedName, u.Lifecycle, u.Revision,
			millis(u.CreatedAt), millis(u.UpdatedAt), nullTime(u.DeletedAt)); err != nil {
			return translateConstraint(err, "user name already exists")
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO xray_user_identities
            (id,instance_id,template_id,statistics_id,kind,created_at) VALUES (?,?,?,?,?,?)`, i.ID.String(), i.InstanceID.String(),
			i.TemplateID.String(), i.StatisticsID, i.Kind, millis(i.CreatedAt)); err != nil {
			return translateConstraint(err, "statistics identity already exists")
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO access_allocations
            (id,user_id,template_id,identity_id,admin_enabled,quota_state,projection_state,observed_present,
             desired_revision,synced_revision,desired_credential_version,synced_credential_version,last_sync_at,
             last_sync_error_code,last_sync_error_summary,created_at,updated_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID.String(), a.UserID.String(), a.TemplateID.String(), a.IdentityID.String(),
			boolInt(a.AdminEnabled), a.QuotaState, a.ProjectionState, nullableBool(a.ObservedPresent), a.DesiredRevision,
			a.SyncedRevision, a.DesiredCredentialVersion, nullableInt64(a.SyncedCredentialVersion), nullTime(a.LastSyncAt),
			nullString(a.LastSyncErrorCode), nullString(a.LastSyncErrorSummary), millis(a.CreatedAt), millis(a.UpdatedAt)); err != nil {
			return err
		}
		// 端口分配与专属入站在同一事务写入；端口冲突由部分唯一索引拒绝，不产生部分状态（FR-008/FR-038）。
		if err := insertDedicatedInbound(ctx, tx.tx, record.Inbound); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO access_credentials
            (id,allocation_id,version,state,key_ciphertext,key_nonce,key_encryption_version,created_at,activated_at,retired_at)
            VALUES (?,?,?,?,?,?,?,?,?,?)`, c.ID.String(), c.AllocationID.String(), c.Version, c.State, c.KeyCiphertext,
			c.KeyNonce, c.KeyEncryptionVersion, millis(c.CreatedAt), nullTime(c.ActivatedAt), nullTime(c.RetiredAt)); err != nil {
			return err
		}
		p := record.Policy
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO quota_policies
            (allocation_id,limit_bytes,reset_day,revision,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
			p.AllocationID.String(), nullableInt64(p.LimitBytes), p.ResetDay, p.Revision, millis(p.CreatedAt), millis(p.UpdatedAt)); err != nil {
			return err
		}
		cycle := record.Cycle
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO quota_cycles
            (id,allocation_id,starts_at_utc,ends_at_utc,timezone_name,reset_day,status,gross_uplink_bytes,
             gross_downlink_bytes,accounted_uplink_bytes,accounted_downlink_bytes,manual_reset_count,opened_at,closed_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, cycle.ID.String(), cycle.AllocationID.String(), millis(cycle.StartsAt),
			millis(cycle.EndsAt), cycle.Timezone, cycle.ResetDay, cycle.Status, cycle.GrossUplinkBytes, cycle.GrossDownlinkBytes,
			cycle.AccountedUplinkBytes, cycle.AccountedDownlinkBytes, cycle.ManualResetCount, millis(cycle.OpenedAt), nullTime(cycle.ClosedAt)); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO allocation_traffic_totals(allocation_id,uplink_bytes,downlink_bytes,updated_at) VALUES (?,0,0,?)`, a.ID.String(), millis(a.CreatedAt)); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO traffic_cursors(allocation_id,uplink_epoch,downlink_epoch,updated_at) VALUES (?,0,0,?)`, a.ID.String(), millis(a.CreatedAt)); err != nil {
			return err
		}
		if err := insertOperation(ctx, tx.tx, record.Operation); err != nil {
			return err
		}
		if err := tx.SaveCommand(ctx, record.Command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, record.Audit)
	})
	return record.User.ID, replay, err
}

func (s *Store) User(ctx context.Context, id domain.ID) (ports.UserRecord, error) {
	return s.loadUser(ctx, `u.id=?`, id.String())
}

func (s *Store) loadUser(ctx context.Context, condition string, args ...any) (ports.UserRecord, error) {
	query := userSelect + ` WHERE ` + condition + ` ORDER BY c.version DESC LIMIT 1`
	row := s.db.Read.QueryRowContext(ctx, query, args...)
	return scanUser(row)
}

const userSelect = `SELECT
    u.id,u.display_name,u.normalized_name,u.lifecycle_state,u.revision,u.created_at,u.updated_at,u.deleted_at,
    i.id,i.instance_id,i.template_id,i.statistics_id,i.kind,i.created_at,
    a.id,a.user_id,a.template_id,a.identity_id,a.admin_enabled,a.quota_state,a.projection_state,a.observed_present,
    a.desired_revision,a.synced_revision,a.desired_credential_version,a.synced_credential_version,a.last_sync_at,
    COALESCE(a.last_sync_error_code,''),COALESCE(a.last_sync_error_summary,''),a.created_at,a.updated_at,
    c.id,c.allocation_id,c.version,c.state,c.key_ciphertext,c.key_nonce,c.key_encryption_version,c.created_at,c.activated_at,c.retired_at,
    t.id,t.instance_id,t.name,t.normalized_name,t.public_host,t.listen_address,t.port_pool_start,t.port_pool_end,
    t.method,t.network,t.compatibility_state,COALESCE(t.compatibility_reason,''),t.last_validated_at,t.revision,
    t.archived_at,t.created_at,t.updated_at,
    d.allocation_id,d.template_id,d.inbound_tag,d.listen_address,d.port,d.server_key_ciphertext,d.server_key_nonce,
    d.key_encryption_version,d.desired_present,d.observed_present,d.last_sync_at,d.released_at,d.created_at,d.updated_at,
    qp.allocation_id,qp.limit_bytes,qp.reset_day,qp.revision,qp.created_at,qp.updated_at,
    qc.id,qc.allocation_id,qc.starts_at_utc,qc.ends_at_utc,qc.timezone_name,qc.reset_day,qc.status,
    qc.gross_uplink_bytes,qc.gross_downlink_bytes,qc.accounted_uplink_bytes,qc.accounted_downlink_bytes,
    qc.manual_reset_count,qc.opened_at,qc.closed_at
    FROM managed_users u
    JOIN access_allocations a ON a.user_id=u.id
    JOIN xray_user_identities i ON i.id=a.identity_id
    JOIN inbound_templates t ON t.id=a.template_id
    JOIN dedicated_inbounds d ON d.allocation_id=a.id
    JOIN access_credentials c ON c.allocation_id=a.id AND c.version=a.desired_credential_version
    JOIN quota_policies qp ON qp.allocation_id=a.id
    JOIN quota_cycles qc ON qc.allocation_id=a.id AND qc.status='open'`

func scanUser(row scanner) (ports.UserRecord, error) {
	var record ports.UserRecord
	var userID, identityID, identityInstanceID, identityTemplateID, allocationID, allocationUserID, allocationTemplateID, allocationIdentityID string
	var credentialID, credentialAllocationID, templateID, templateInstanceID, policyAllocationID, cycleID, cycleAllocationID string
	var inboundAllocationID, inboundTemplateID string
	var poolStart, poolEnd int
	var userCreated, userUpdated, identityCreated, allocationCreated, allocationUpdated, credentialCreated int64
	var templateCreated, templateUpdated, policyCreated, policyUpdated, starts, ends, opened int64
	var inboundCreated, inboundUpdated, inboundDesired int64
	var userDeleted, lastSync, credentialActivated, credentialRetired, templateValidated, templateArchived, cycleClosed sql.NullInt64
	var observed, syncedCredential, keyVersion, limit sql.NullInt64
	var inboundObserved, inboundLastSync, inboundReleased sql.NullInt64
	err := row.Scan(&userID, &record.User.DisplayName, &record.User.NormalizedName, &record.User.Lifecycle, &record.User.Revision,
		&userCreated, &userUpdated, &userDeleted, &identityID, &identityInstanceID, &identityTemplateID,
		&record.Identity.StatisticsID, &record.Identity.Kind, &identityCreated, &allocationID, &allocationUserID,
		&allocationTemplateID, &allocationIdentityID, &record.Allocation.AdminEnabled, &record.Allocation.QuotaState,
		&record.Allocation.ProjectionState, &observed, &record.Allocation.DesiredRevision, &record.Allocation.SyncedRevision,
		&record.Allocation.DesiredCredentialVersion, &syncedCredential, &lastSync, &record.Allocation.LastSyncErrorCode,
		&record.Allocation.LastSyncErrorSummary, &allocationCreated, &allocationUpdated, &credentialID, &credentialAllocationID,
		&record.Credential.Version, &record.Credential.State, &record.Credential.KeyCiphertext, &record.Credential.KeyNonce,
		&keyVersion, &credentialCreated, &credentialActivated, &credentialRetired,
		&templateID, &templateInstanceID, &record.Template.Template.Name, &record.Template.Template.NormalizedName,
		&record.Template.Template.PublicHost, &record.Template.Template.ListenAddress, &poolStart, &poolEnd,
		&record.Template.Template.Method, &record.Template.Template.Network, &record.Template.Template.Compatibility,
		&record.Template.Template.CompatibilityReason, &templateValidated, &record.Template.Template.Revision,
		&templateArchived, &templateCreated, &templateUpdated,
		&inboundAllocationID, &inboundTemplateID, &record.Inbound.Inbound.InboundTag, &record.Inbound.Inbound.ListenAddress,
		&record.Inbound.Inbound.Port, &record.Inbound.ServerKeyCiphertext, &record.Inbound.ServerKeyNonce,
		&record.Inbound.KeyEncryptionVersion, &inboundDesired, &inboundObserved, &inboundLastSync, &inboundReleased,
		&inboundCreated, &inboundUpdated,
		&policyAllocationID, &limit, &record.Policy.ResetDay, &record.Policy.Revision,
		&policyCreated, &policyUpdated, &cycleID, &cycleAllocationID, &starts, &ends, &record.Cycle.Timezone,
		&record.Cycle.ResetDay, &record.Cycle.Status, &record.Cycle.GrossUplinkBytes, &record.Cycle.GrossDownlinkBytes,
		&record.Cycle.AccountedUplinkBytes, &record.Cycle.AccountedDownlinkBytes, &record.Cycle.ManualResetCount, &opened, &cycleClosed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, &domain.NotFoundError{Resource: "user"}
		}
		return record, err
	}
	record.User.ID, record.User.CreatedAt, record.User.UpdatedAt = domain.ID(userID), fromMillis(userCreated), fromMillis(userUpdated)
	setTime(&record.User.DeletedAt, userDeleted)
	record.Identity.ID, record.Identity.InstanceID, record.Identity.TemplateID = domain.ID(identityID), domain.ID(identityInstanceID), domain.ID(identityTemplateID)
	record.Identity.CreatedAt = fromMillis(identityCreated)
	record.Allocation.ID, record.Allocation.UserID, record.Allocation.TemplateID, record.Allocation.IdentityID = domain.ID(allocationID), domain.ID(allocationUserID), domain.ID(allocationTemplateID), domain.ID(allocationIdentityID)
	record.Allocation.CreatedAt, record.Allocation.UpdatedAt = fromMillis(allocationCreated), fromMillis(allocationUpdated)
	setBool(&record.Allocation.ObservedPresent, observed)
	setInt64(&record.Allocation.SyncedCredentialVersion, syncedCredential)
	setTime(&record.Allocation.LastSyncAt, lastSync)
	record.Credential.ID, record.Credential.AllocationID = domain.ID(credentialID), domain.ID(credentialAllocationID)
	record.Credential.CreatedAt = fromMillis(credentialCreated)
	if keyVersion.Valid {
		record.Credential.KeyEncryptionVersion = keyVersion.Int64
	}
	setTime(&record.Credential.ActivatedAt, credentialActivated)
	setTime(&record.Credential.RetiredAt, credentialRetired)
	record.Template.Template.ID, record.Template.Template.InstanceID = domain.ID(templateID), domain.ID(templateInstanceID)
	record.Template.Template.Pool = domain.PortPool{Start: poolStart, End: poolEnd}
	record.Template.Template.CreatedAt, record.Template.Template.UpdatedAt = fromMillis(templateCreated), fromMillis(templateUpdated)
	setTime(&record.Template.Template.LastValidatedAt, templateValidated)
	setTime(&record.Template.Template.ArchivedAt, templateArchived)
	record.Inbound.Inbound.AllocationID, record.Inbound.Inbound.TemplateID = domain.ID(inboundAllocationID), domain.ID(inboundTemplateID)
	record.Inbound.Inbound.DesiredPresent = inboundDesired != 0
	setBool(&record.Inbound.Inbound.ObservedPresent, inboundObserved)
	setTime(&record.Inbound.Inbound.LastSyncAt, inboundLastSync)
	setTime(&record.Inbound.Inbound.ReleasedAt, inboundReleased)
	record.Inbound.Inbound.CreatedAt, record.Inbound.Inbound.UpdatedAt = fromMillis(inboundCreated), fromMillis(inboundUpdated)
	record.Policy.AllocationID, record.Policy.CreatedAt, record.Policy.UpdatedAt = domain.ID(policyAllocationID), fromMillis(policyCreated), fromMillis(policyUpdated)
	if limit.Valid {
		value := limit.Int64
		record.Policy.LimitBytes = &value
	}
	record.Cycle.ID, record.Cycle.AllocationID = domain.ID(cycleID), domain.ID(cycleAllocationID)
	record.Cycle.StartsAt, record.Cycle.EndsAt, record.Cycle.OpenedAt = fromMillis(starts), fromMillis(ends), fromMillis(opened)
	setTime(&record.Cycle.ClosedAt, cycleClosed)
	return record, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	return boolInt(*value)
}
func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
func setTime(target **time.Time, source sql.NullInt64) {
	if source.Valid {
		value := fromMillis(source.Int64)
		*target = &value
	}
}
func setInt64(target **int64, source sql.NullInt64) {
	if source.Valid {
		value := source.Int64
		*target = &value
	}
}
func setBool(target **bool, source sql.NullInt64) {
	if source.Valid {
		value := source.Int64 != 0
		*target = &value
	}
}

// ListUsers 按规范化名称模糊匹配并可按派生状态筛选；状态判定在 Go 中完成（≤20 用户）。
// ListUsers 按名称或端口检索（FR-014）：纯数字的查询同时匹配专属端口，便于从端口反查用户。
func (s *Store) ListUsers(ctx context.Context, filter ports.UserFilter) ([]ports.UserRecord, error) {
	query := userSelect + ` WHERE (? = '' OR u.normalized_name LIKE ? OR (? != 0 AND d.port = ?))
        AND (? = 1 OR u.deleted_at IS NULL) ORDER BY u.normalized_name, u.id`
	normalized, _ := domain.NormalizeDisplayName(filter.Query)
	pattern := "%" + strings.ReplaceAll(strings.ReplaceAll(normalized, "%", ""), "_", "") + "%"
	if strings.TrimSpace(filter.Query) == "" {
		normalized = ""
	}
	port, _ := strconv.Atoi(strings.TrimSpace(filter.Query))
	rows, err := s.db.Read.QueryContext(ctx, query, normalized, pattern, port, port, boolInt(filter.IncludeDeleted))
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
		if !matchesStatus(record, filter.Status) {
			continue
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func matchesStatus(record ports.UserRecord, status string) bool {
	state := record.Allocation.DisplayState(record.User)
	switch status {
	case "":
		return true
	case "pending":
		return record.User.Lifecycle != domain.LifecycleDeleted && record.Allocation.PendingSync()
	case string(domain.DisplayActive):
		// 业务筛选“启用”= 管理员启用且配额内（含启用中/待同步），与“待同步”筛选正交（FR-008）。
		return record.Allocation.DesiredPresent(record.User)
	case string(domain.DisplayDisabled):
		return state == domain.DisplayDisabled || state == domain.DisplayDisabling
	case string(domain.DisplayQuotaExceeded):
		return state == domain.DisplayQuotaExceeded || state == domain.DisplayQuotaDisabling
	case string(domain.DisplayDeleted):
		return state == domain.DisplayDeleted
	default:
		return false
	}
}

// RotateCredential 保存下一版本 pending 凭证并写入 remove_old 阶段的轮换操作；同一时间只允许一次轮换进行。
func (s *Store) RotateCredential(ctx context.Context, record ports.RotationRecord) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, record.Command)
		if err != nil || replay {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE managed_users SET revision=revision+1,updated_at=? WHERE id=? AND revision=? AND deleted_at IS NULL`,
			millis(record.Now), record.UserID.String(), record.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return &domain.ConflictError{Message: "user changed since the page was loaded"}
		}
		var pending int
		if err := tx.tx.QueryRowContext(ctx, `SELECT count(*) FROM access_credentials WHERE allocation_id=? AND state='pending'`, record.AllocationID.String()).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return &domain.ConflictError{Message: "credential rotation already in progress"}
		}
		c := record.Credential
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO access_credentials
            (id,allocation_id,version,state,key_ciphertext,key_nonce,key_encryption_version,created_at,activated_at,retired_at)
            VALUES (?,?,?,?,?,?,?,?,NULL,NULL)`, c.ID.String(), c.AllocationID.String(), c.Version, c.State, c.KeyCiphertext, c.KeyNonce,
			c.KeyEncryptionVersion, millis(c.CreatedAt)); err != nil {
			return translateConstraint(err, "credential rotation already in progress")
		}
		op := record.Operation
		if err := supersedeOlder(ctx, tx.tx, op.AllocationID, op.DesiredRevision, record.Now); err != nil {
			return err
		}
		if err := insertOperation(ctx, tx.tx, op); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET desired_credential_version=?,desired_revision=?,projection_state='pending',updated_at=? WHERE id=?`,
			c.Version, op.DesiredRevision, millis(record.Now), record.AllocationID.String()); err != nil {
			return err
		}
		if err := tx.SaveCommand(ctx, record.Command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, record.Audit)
	})
	return replay, err
}

// SoftDeleteUser 把用户标记为 deleted、清除启用意图并写入移除操作；密钥销毁在移除确认事务中完成。
// SoftDeleteUser 软删除用户并排队移除入站；端口在移除确认事务内释放（见 ConfirmSync）。
func (s *Store) SoftDeleteUser(ctx context.Context, record ports.DeleteRecord) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, record.Command)
		if err != nil || replay {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE managed_users SET lifecycle_state='deleted',deleted_at=?,revision=revision+1,updated_at=?
            WHERE id=? AND revision=? AND deleted_at IS NULL`, millis(record.Now), millis(record.Now), record.UserID.String(), record.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return &domain.ConflictError{Message: "user changed since the page was loaded"}
		}
		op := record.Operation
		if err := supersedeOlder(ctx, tx.tx, op.AllocationID, op.DesiredRevision, record.Now); err != nil {
			return err
		}
		if err := insertOperation(ctx, tx.tx, op); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE access_allocations SET admin_enabled=0,desired_revision=?,projection_state='pending',updated_at=? WHERE id=?`,
			op.DesiredRevision, millis(record.Now), record.AllocationID.String()); err != nil {
			return err
		}
		if err := tx.SaveCommand(ctx, record.Command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, record.Audit)
	})
	return replay, err
}
