package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func (s *Store) ManagedInstance(ctx context.Context) (ports.ManagedInstanceRecord, error) {
	row := s.db.Read.QueryRowContext(ctx, `SELECT id,name,api_endpoint,supported_runtime_version,health_state,
        COALESCE(boot_epoch,''),last_success_at,COALESCE(last_error_code,''),COALESCE(last_error_summary,''),updated_at
        FROM managed_xray_instances WHERE singleton=1`)
	var result ports.ManagedInstanceRecord
	var id string
	var success sql.NullInt64
	var updated int64
	if err := row.Scan(&id, &result.Name, &result.APIEndpoint, &result.SupportedRuntimeVersion, &result.HealthState,
		&result.BootEpoch, &success, &result.LastErrorCode, &result.LastErrorSummary, &updated); err != nil {
		return result, err
	}
	result.ID = domain.ID(id)
	result.UpdatedAt = fromMillis(updated)
	if success.Valid {
		value := fromMillis(success.Int64)
		result.LastSuccessAt = &value
	}
	return result, nil
}

func (s *Store) SetInstanceHealth(ctx context.Context, health, bootEpoch, errorCode string, successAt *time.Time, summary string, now time.Time) error {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE managed_xray_instances SET health_state=?,boot_epoch=?,last_success_at=?,
        last_error_code=?,last_error_summary=?,updated_at=? WHERE singleton=1`, health, nullString(bootEpoch),
		nullTime(successAt), nullString(errorCode), nullString(summary), millis(now))
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.NotFoundError{Resource: "Xray instance"}
	}
	return nil
}

// MarkInstanceHealthy 记录一次成功的 Xray 交互；bootEpoch 为空时保留原值。
func (s *Store) MarkInstanceHealthy(ctx context.Context, bootEpoch string, now time.Time) error {
	_, err := s.db.Write.ExecContext(ctx, `UPDATE managed_xray_instances SET health_state='healthy',
        boot_epoch=COALESCE(?,boot_epoch),last_success_at=?,last_error_code=NULL,last_error_summary=NULL,updated_at=? WHERE singleton=1`,
		nullString(bootEpoch), millis(now), millis(now))
	return err
}

// MarkInstanceUnreachable 记录 Xray 不可达，保留最后成功时间以供页面展示。
func (s *Store) MarkInstanceUnreachable(ctx context.Context, code, summary string, now time.Time) error {
	_, err := s.db.Write.ExecContext(ctx, `UPDATE managed_xray_instances SET health_state='unreachable',
        last_error_code=?,last_error_summary=?,updated_at=? WHERE singleton=1`, nullString(code), nullString(summary), millis(now))
	return err
}

func commandReplay(ctx context.Context, tx *txStore, command domain.DomainCommand) (bool, error) {
	existing, err := tx.FindCommand(ctx, command.ID)
	if err != nil {
		return false, err
	}
	if existing == nil {
		return false, nil
	}
	if !bytes.Equal(existing.RequestFingerprint, command.RequestFingerprint) {
		return false, &domain.ConflictError{Message: "request identifier was reused with different input"}
	}
	return true, nil
}

func (s *Store) CreateProfile(ctx context.Context, record ports.ProfileRecord, command domain.DomainCommand, audit domain.AuditEvent) (domain.ID, bool, error) {
	id, replayed := record.Profile.ID, false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		existing, err := tx.FindCommand(ctx, command.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !bytes.Equal(existing.RequestFingerprint, command.RequestFingerprint) {
				return &domain.ConflictError{Message: "request identifier was reused with different input"}
			}
			id, replayed = existing.TargetID, true
			return nil
		}
		p := record.Profile
		_, err = tx.tx.ExecContext(ctx, `INSERT INTO access_profiles
            (id,instance_id,name,normalized_name,inbound_tag,public_host,public_port,method,network,
             server_key_ciphertext,server_key_nonce,key_encryption_version,bootstrap_statistics_id,
             compatibility_state,compatibility_reason,last_validated_at,revision,archived_at,created_at,updated_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID.String(), p.InstanceID.String(), p.Name,
			p.NormalizedName, p.InboundTag, p.PublicHost, p.PublicPort, p.Method, p.Network, record.ServerKeyCiphertext,
			record.ServerKeyNonce, record.KeyEncryptionVersion, p.BootstrapStatisticsID, p.Compatibility,
			nullString(p.CompatibilityReason), nullTime(p.LastValidatedAt), p.Revision, nullTime(p.ArchivedAt),
			millis(p.CreatedAt), millis(p.UpdatedAt))
		if err != nil {
			return translateConstraint(err, "profile already exists")
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
	return id, replayed, err
}

func (s *Store) UpdateProfile(ctx context.Context, record ports.ProfileRecord, match ports.RevisionMatch, command domain.DomainCommand, audit domain.AuditEvent) error {
	return s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		replay, err := commandReplay(ctx, tx, command)
		if err != nil || replay {
			return err
		}
		p := record.Profile
		var currentTag, currentMethod, currentBootstrap string
		var liveAllocations, openOperations, openRemovals int
		// 契约字段（inbound_tag/method/bootstrap）只在旧入站上没有任何“可能仍存在”的面板身份时才允许变更：
		// 未删除用户、已软删除但移除尚未确认 absent 的分配、未终结的同步操作与漂移移除都算在内；
		// permanent_failed 的漂移移除不算安全终结——身份可能仍在旧入站——直到同一身份之后有一次成功移除（FR-006/FR-012/FR-020）。
		if err := tx.tx.QueryRowContext(ctx, `SELECT inbound_tag,method,bootstrap_statistics_id,
            (SELECT count(*) FROM access_allocations a JOIN managed_users u ON u.id=a.user_id WHERE a.profile_id=access_profiles.id
               AND (u.deleted_at IS NULL OR a.projection_state<>'absent' OR a.synced_revision<>a.desired_revision OR COALESCE(a.observed_present,0)=1)),
            (SELECT count(*) FROM synchronization_operations o JOIN access_allocations a ON a.id=o.allocation_id
               WHERE a.profile_id=access_profiles.id AND o.state IN ('pending','leased','retry_wait')),
            (SELECT count(*) FROM drift_removals d WHERE d.profile_id=access_profiles.id AND (d.state IN ('pending','leased','retry_wait')
               OR (d.state='permanent_failed' AND NOT EXISTS (SELECT 1 FROM drift_removals s WHERE s.profile_id=d.profile_id
                   AND s.statistics_id=d.statistics_id AND s.state='succeeded' AND s.created_at>=d.created_at))))
            FROM access_profiles WHERE id=?`, p.ID.String()).Scan(&currentTag, &currentMethod, &currentBootstrap, &liveAllocations, &openOperations, &openRemovals); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &domain.NotFoundError{Resource: "profile"}
			}
			return err
		}
		contractChanged := currentTag != p.InboundTag || currentMethod != p.Method || currentBootstrap != p.BootstrapStatisticsID
		if contractChanged && (liveAllocations > 0 || openOperations > 0 || openRemovals > 0) {
			return &domain.ConflictError{Message: "profile still has users, unconfirmed removals or pending synchronization on the current inbound; " +
				"inbound tag, method and bootstrap identity cannot change until they are confirmed absent"}
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE access_profiles SET name=?,normalized_name=?,inbound_tag=?,public_host=?,
            public_port=?,method=?,network=?,server_key_ciphertext=?,server_key_nonce=?,key_encryption_version=?,
            bootstrap_statistics_id=?,compatibility_state=?,compatibility_reason=?,last_validated_at=?,revision=revision+1,updated_at=?
            WHERE id=? AND revision=? AND archived_at IS NULL`, p.Name, p.NormalizedName, p.InboundTag, p.PublicHost,
			p.PublicPort, p.Method, p.Network, record.ServerKeyCiphertext, record.ServerKeyNonce, record.KeyEncryptionVersion,
			p.BootstrapStatisticsID, p.Compatibility, nullString(p.CompatibilityReason), nullTime(p.LastValidatedAt),
			millis(p.UpdatedAt), p.ID.String(), match.Expected)
		if err != nil {
			return translateConstraint(err, "profile already exists")
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return &domain.ConflictError{Message: "profile changed since the page was loaded"}
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
}

func (s *Store) Profile(ctx context.Context, id domain.ID) (ports.ProfileRecord, error) {
	row := s.db.Read.QueryRowContext(ctx, profileSelect+` WHERE p.id=?`, id.String())
	return scanProfile(row)
}

func (s *Store) Profiles(ctx context.Context, compatibleOnly bool) ([]ports.ProfileRecord, error) {
	query := profileSelect + ` WHERE p.archived_at IS NULL`
	if compatibleOnly {
		query += ` AND p.compatibility_state='compatible'`
	}
	query += ` ORDER BY p.normalized_name,p.id`
	rows, err := s.db.Read.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.ProfileRecord
	for rows.Next() {
		record, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) ArchiveProfile(ctx context.Context, id domain.ID, revision domain.Revision, now time.Time) error {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE access_profiles SET archived_at=?,revision=revision+1,updated_at=?
        WHERE id=? AND revision=? AND archived_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM access_allocations a JOIN managed_users u ON u.id=a.user_id
                          WHERE a.profile_id=access_profiles.id AND u.deleted_at IS NULL)`,
		millis(now), millis(now), id.String(), revision)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.ConflictError{Message: "profile changed or still has managed users"}
	}
	return nil
}

const profileSelect = `SELECT p.id,p.instance_id,p.name,p.normalized_name,p.inbound_tag,p.public_host,p.public_port,
    p.method,p.network,p.server_key_ciphertext,p.server_key_nonce,p.key_encryption_version,p.bootstrap_statistics_id,
    p.compatibility_state,COALESCE(p.compatibility_reason,''),p.last_validated_at,p.revision,p.archived_at,p.created_at,p.updated_at
    FROM access_profiles p`

type scanner interface{ Scan(...any) error }

func scanProfile(row scanner) (ports.ProfileRecord, error) {
	var record ports.ProfileRecord
	var id, instanceID string
	var validated, archived sql.NullInt64
	var created, updated int64
	err := row.Scan(&id, &instanceID, &record.Profile.Name, &record.Profile.NormalizedName, &record.Profile.InboundTag,
		&record.Profile.PublicHost, &record.Profile.PublicPort, &record.Profile.Method, &record.Profile.Network,
		&record.ServerKeyCiphertext, &record.ServerKeyNonce, &record.KeyEncryptionVersion,
		&record.Profile.BootstrapStatisticsID, &record.Profile.Compatibility, &record.Profile.CompatibilityReason,
		&validated, &record.Profile.Revision, &archived, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, &domain.NotFoundError{Resource: "profile"}
		}
		return record, err
	}
	record.Profile.ID, record.Profile.InstanceID = domain.ID(id), domain.ID(instanceID)
	record.Profile.CreatedAt, record.Profile.UpdatedAt = fromMillis(created), fromMillis(updated)
	if validated.Valid {
		value := fromMillis(validated.Int64)
		record.Profile.LastValidatedAt = &value
	}
	if archived.Valid {
		value := fromMillis(archived.Int64)
		record.Profile.ArchivedAt = &value
	}
	return record, nil
}

func (s *Store) SetProfileCompatibility(ctx context.Context, id domain.ID, state domain.CompatibilityState, reason string, now time.Time) error {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE access_profiles SET compatibility_state=?,compatibility_reason=?,last_validated_at=?,updated_at=? WHERE id=?`,
		state, nullString(reason), millis(now), millis(now), id.String())
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.NotFoundError{Resource: "profile"}
	}
	return nil
}

// RegisterBootstrapIdentity 注册保留初始用户身份：同 profile 重放幂等，跨 profile 复用同一统计标识返回冲突。
func (s *Store) RegisterBootstrapIdentity(ctx context.Context, identity domain.XrayUserIdentity) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingProfile, existingKind string
	err = tx.QueryRowContext(ctx, `SELECT profile_id,kind FROM xray_user_identities WHERE instance_id=? AND statistics_id=?`,
		identity.InstanceID.String(), identity.StatisticsID).Scan(&existingProfile, &existingKind)
	switch {
	case err == nil:
		if existingProfile != identity.ProfileID.String() || existingKind != string(identity.Kind) {
			return &domain.ConflictError{Message: "bootstrap identity is already registered by another profile"}
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xray_user_identities(id,instance_id,profile_id,statistics_id,kind,created_at) VALUES (?,?,?,?,?,?)`,
		identity.ID.String(), identity.InstanceID.String(), identity.ProfileID.String(), identity.StatisticsID, identity.Kind, millis(identity.CreatedAt)); err != nil {
		return translateConstraint(err, "bootstrap identity is already registered by another profile")
	}
	return tx.Commit()
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func translateConstraint(err error, message string) error {
	if err != nil && (bytes.Contains([]byte(err.Error()), []byte("UNIQUE constraint")) || bytes.Contains([]byte(err.Error()), []byte("constraint failed"))) {
		return &domain.ConflictError{Message: message}
	}
	return err
}

// CompleteProfileValidation 在一个事务中按 revision 条件提交验证结果、实例健康、bootstrap 身份与审计；
// revision 已变化（验证期间被编辑或重新验证）时丢弃结果并返回 false。
func (s *Store) CompleteProfileValidation(ctx context.Context, outcome ports.ValidationOutcome) (bool, error) {
	applied := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		state, reason, health, code, summary, success := outcome.State, outcome.Reason, outcome.Health, outcome.ErrorCode, outcome.ErrorSummary, outcome.SuccessAt
		var registerBootstrap *domain.XrayUserIdentity
		if outcome.Bootstrap != nil && state == domain.CompatibilityCompatible {
			b := outcome.Bootstrap
			var existingProfile, existingKind string
			err := tx.tx.QueryRowContext(ctx, `SELECT profile_id,kind FROM xray_user_identities WHERE instance_id=? AND statistics_id=?`,
				b.InstanceID.String(), b.StatisticsID).Scan(&existingProfile, &existingKind)
			switch {
			case err == nil:
				if existingProfile != b.ProfileID.String() || existingKind != string(b.Kind) {
					// 跨 profile 复用同一 bootstrap 统计标识：全实例唯一性被破坏，不得标为 compatible（contract gate 9）。
					state, reason = domain.CompatibilityIncompatible, "bootstrap identity is already registered by another profile"
					health, code, summary, success = "incompatible", "incompatible_profile", reason, nil
				}
			case errors.Is(err, sql.ErrNoRows):
				registerBootstrap = b
			default:
				return err
			}
		}
		// 先做 revision 条件更新：结果过期时事务内不得留下任何写入（含 bootstrap 身份）。
		result, err := tx.tx.ExecContext(ctx, `UPDATE access_profiles SET compatibility_state=?,compatibility_reason=?,last_validated_at=?,updated_at=?
            WHERE id=? AND revision=? AND archived_at IS NULL`, state, nullString(reason), millis(outcome.ValidatedAt), millis(outcome.ValidatedAt),
			outcome.ProfileID.String(), outcome.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return nil
		}
		applied = true
		if registerBootstrap != nil {
			b := registerBootstrap
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO xray_user_identities(id,instance_id,profile_id,statistics_id,kind,created_at) VALUES (?,?,?,?,?,?)`,
				b.ID.String(), b.InstanceID.String(), b.ProfileID.String(), b.StatisticsID, b.Kind, millis(b.CreatedAt)); err != nil {
				return err
			}
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE managed_xray_instances SET health_state=?,boot_epoch=COALESCE(?,boot_epoch),
            last_success_at=COALESCE(?,last_success_at),last_error_code=?,last_error_summary=?,updated_at=? WHERE singleton=1`,
			health, nullString(outcome.BootEpoch), nullTime(success), nullString(code), nullString(summary), millis(outcome.ValidatedAt)); err != nil {
			return err
		}
		audit := outcome.Audit
		audit.SafeSummary = reason
		if state != domain.CompatibilityCompatible {
			audit.Result = domain.AuditFailed
		}
		return tx.AppendAudit(ctx, audit)
	})
	return applied, err
}

// RequestRevalidation 以 revision 条件把 profile 置回 unverified 并递增 revision；同请求重放幂等。
func (s *Store) RequestRevalidation(ctx context.Context, id domain.ID, expected domain.Revision, command domain.DomainCommand, audit domain.AuditEvent) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, command)
		if err != nil || replay {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE access_profiles SET compatibility_state='unverified',compatibility_reason=NULL,last_validated_at=NULL,
            revision=revision+1,updated_at=? WHERE id=? AND revision=? AND archived_at IS NULL`, millis(audit.OccurredAt), id.String(), expected)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return &domain.ConflictError{Message: "profile changed since the page was loaded"}
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
	return replay, err
}
