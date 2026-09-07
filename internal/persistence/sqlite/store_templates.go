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

// CreateTemplate 登记入站模板；同请求重放返回原目标。模板不再持有服务端密钥（FR-004）。
func (s *Store) CreateTemplate(ctx context.Context, record ports.TemplateRecord, command domain.DomainCommand, audit domain.AuditEvent) (domain.ID, bool, error) {
	id, replayed := record.Template.ID, false
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
		t := record.Template
		_, err = tx.tx.ExecContext(ctx, `INSERT INTO inbound_templates
            (id,instance_id,name,normalized_name,public_host,listen_address,port_pool_start,port_pool_end,method,network,
             compatibility_state,compatibility_reason,last_validated_at,revision,archived_at,created_at,updated_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID.String(), t.InstanceID.String(), t.Name, t.NormalizedName,
			t.PublicHost, t.ListenAddress, t.Pool.Start, t.Pool.End, t.Method, t.Network, t.Compatibility,
			nullString(t.CompatibilityReason), nullTime(t.LastValidatedAt), t.Revision, nullTime(t.ArchivedAt),
			millis(t.CreatedAt), millis(t.UpdatedAt))
		if err != nil {
			return translateConstraint(err, "inbound template already exists")
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
	return id, replayed, err
}

// UpdateTemplate 更新入站模板。
//
// 约束：契约字段（method、listen_address）决定已创建入站的密钥长度与绑定地址，只有在该模板下没有任何
// “可能仍存在”的面板入站时才允许变更：未删除用户、投影未确认 absent 的分配、未终结的同步操作，
// 以及尚未被后续意图取代的 permanent_failed 漂移移除都算在内（FR-006/FR-012/FR-020）。
// 端口池可以随时调整；缩小后落在池外的既有分配保持可用，只在界面标识（US3 场景 7）。
func (s *Store) UpdateTemplate(ctx context.Context, record ports.TemplateRecord, match ports.RevisionMatch, command domain.DomainCommand, audit domain.AuditEvent) error {
	return s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		replay, err := commandReplay(ctx, tx, command)
		if err != nil || replay {
			return err
		}
		t := record.Template
		var currentMethod, currentListen string
		var liveAllocations, openOperations, openRemovals int
		if err := tx.tx.QueryRowContext(ctx, `SELECT method,listen_address,
            (SELECT count(*) FROM access_allocations a JOIN managed_users u ON u.id=a.user_id WHERE a.template_id=inbound_templates.id
               AND (u.deleted_at IS NULL OR a.projection_state<>'absent' OR a.synced_revision<>a.desired_revision OR COALESCE(a.observed_present,0)=1)),
            (SELECT count(*) FROM synchronization_operations o JOIN access_allocations a ON a.id=o.allocation_id
               WHERE a.template_id=inbound_templates.id AND o.state IN ('pending','leased','retry_wait')),
            (SELECT count(*) FROM drift_removals d WHERE d.template_id=inbound_templates.id AND (d.state IN ('pending','leased','retry_wait')
               OR (d.state='permanent_failed' AND d.superseded_by IS NULL)))
            FROM inbound_templates WHERE id=?`, t.ID.String()).Scan(&currentMethod, &currentListen, &liveAllocations, &openOperations, &openRemovals); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &domain.NotFoundError{Resource: "inbound template"}
			}
			return err
		}
		contractChanged := currentMethod != t.Method || currentListen != t.ListenAddress
		if contractChanged && (liveAllocations > 0 || openOperations > 0 || openRemovals > 0) {
			return &domain.ConflictError{Message: "template still has users, unconfirmed removals or pending synchronization; " +
				"method and listen address cannot change until they are confirmed absent"}
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE inbound_templates SET name=?,normalized_name=?,public_host=?,listen_address=?,
            port_pool_start=?,port_pool_end=?,method=?,network=?,compatibility_state=?,compatibility_reason=?,last_validated_at=?,
            revision=revision+1,updated_at=? WHERE id=? AND revision=? AND archived_at IS NULL`,
			t.Name, t.NormalizedName, t.PublicHost, t.ListenAddress, t.Pool.Start, t.Pool.End, t.Method, t.Network,
			t.Compatibility, nullString(t.CompatibilityReason), nullTime(t.LastValidatedAt), millis(t.UpdatedAt),
			t.ID.String(), match.Expected)
		if err != nil {
			return translateConstraint(err, "inbound template already exists")
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return &domain.ConflictError{Message: "inbound template changed since the page was loaded"}
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
}

func (s *Store) Template(ctx context.Context, id domain.ID) (ports.TemplateRecord, error) {
	row := s.db.Read.QueryRowContext(ctx, templateSelect+` WHERE t.id=?`, id.String())
	return scanTemplate(row)
}

func (s *Store) Templates(ctx context.Context, compatibleOnly bool) ([]ports.TemplateRecord, error) {
	query := templateSelect + ` WHERE t.archived_at IS NULL`
	if compatibleOnly {
		query += ` AND t.compatibility_state='compatible'`
	}
	query += ` ORDER BY t.normalized_name,t.id`
	rows, err := s.db.Read.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ports.TemplateRecord
	for rows.Next() {
		record, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) ArchiveTemplate(ctx context.Context, id domain.ID, revision domain.Revision, now time.Time) error {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE inbound_templates SET archived_at=?,revision=revision+1,updated_at=?
        WHERE id=? AND revision=? AND archived_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM access_allocations a JOIN managed_users u ON u.id=a.user_id
                          WHERE a.template_id=inbound_templates.id AND u.deleted_at IS NULL)`,
		millis(now), millis(now), id.String(), revision)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.ConflictError{Message: "inbound template changed or still has managed users"}
	}
	return nil
}

const templateSelect = `SELECT t.id,t.instance_id,t.name,t.normalized_name,t.public_host,t.listen_address,
    t.port_pool_start,t.port_pool_end,t.method,t.network,t.compatibility_state,COALESCE(t.compatibility_reason,''),
    t.last_validated_at,t.revision,t.archived_at,t.created_at,t.updated_at,COALESCE(t.validated_boot_epoch,'')
    FROM inbound_templates t`

type scanner interface{ Scan(...any) error }

func scanTemplate(row scanner) (ports.TemplateRecord, error) {
	var record ports.TemplateRecord
	var id, instanceID string
	var poolStart, poolEnd int
	var validated, archived sql.NullInt64
	var created, updated int64
	err := row.Scan(&id, &instanceID, &record.Template.Name, &record.Template.NormalizedName, &record.Template.PublicHost,
		&record.Template.ListenAddress, &poolStart, &poolEnd, &record.Template.Method, &record.Template.Network,
		&record.Template.Compatibility, &record.Template.CompatibilityReason, &validated, &record.Template.Revision,
		&archived, &created, &updated, &record.Template.ValidatedBootEpoch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, &domain.NotFoundError{Resource: "inbound template"}
		}
		return record, err
	}
	record.Template.ID, record.Template.InstanceID = domain.ID(id), domain.ID(instanceID)
	record.Template.Pool = domain.PortPool{Start: poolStart, End: poolEnd}
	record.Template.CreatedAt, record.Template.UpdatedAt = fromMillis(created), fromMillis(updated)
	if validated.Valid {
		value := fromMillis(validated.Int64)
		record.Template.LastValidatedAt = &value
	}
	if archived.Valid {
		value := fromMillis(archived.Int64)
		record.Template.ArchivedAt = &value
	}
	return record, nil
}

func (s *Store) SetTemplateCompatibility(ctx context.Context, id domain.ID, state domain.CompatibilityState, reason string, now time.Time) error {
	result, err := s.db.Write.ExecContext(ctx, `UPDATE inbound_templates SET compatibility_state=?,compatibility_reason=?,last_validated_at=?,updated_at=? WHERE id=?`,
		state, nullString(reason), millis(now), millis(now), id.String())
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &domain.NotFoundError{Resource: "inbound template"}
	}
	return nil
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

// CompleteTemplateValidation 在一个事务中按 revision 条件提交验证结果、实例健康与审计；
// revision 已变化（验证期间被编辑或重新验证）时丢弃结果并返回 false。
func (s *Store) CompleteTemplateValidation(ctx context.Context, outcome ports.ValidationOutcome) (bool, error) {
	applied := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		// 兼容结论与它所依据的启动纪元一起落库：不兼容/不可达时清空，避免旧世代的证据被误用。
		validatedEpoch := ""
		if outcome.State == domain.CompatibilityCompatible {
			validatedEpoch = outcome.BootEpoch
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE inbound_templates SET compatibility_state=?,compatibility_reason=?,
            last_validated_at=?,validated_boot_epoch=?,updated_at=?
            WHERE id=? AND revision=? AND archived_at IS NULL`, outcome.State, nullString(outcome.Reason),
			millis(outcome.ValidatedAt), nullString(validatedEpoch), millis(outcome.ValidatedAt),
			outcome.TemplateID.String(), outcome.ExpectedRevision)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return nil
		}
		applied = true
		if _, err := tx.tx.ExecContext(ctx, `UPDATE managed_xray_instances SET health_state=?,boot_epoch=COALESCE(?,boot_epoch),
            last_success_at=COALESCE(?,last_success_at),last_error_code=?,last_error_summary=?,updated_at=? WHERE singleton=1`,
			outcome.Health, nullString(outcome.BootEpoch), nullTime(outcome.SuccessAt), nullString(outcome.ErrorCode),
			nullString(outcome.ErrorSummary), millis(outcome.ValidatedAt)); err != nil {
			return err
		}
		audit := outcome.Audit
		audit.SafeSummary = outcome.Reason
		if outcome.State != domain.CompatibilityCompatible {
			audit.Result = domain.AuditFailed
		}
		return tx.AppendAudit(ctx, audit)
	})
	return applied, err
}

// RequestRevalidation 以 revision 条件把模板置回 unverified 并递增 revision；同请求重放幂等。
func (s *Store) RequestRevalidation(ctx context.Context, id domain.ID, expected domain.Revision, command domain.DomainCommand, audit domain.AuditEvent) (bool, error) {
	replay := false
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		var err error
		replay, err = commandReplay(ctx, tx, command)
		if err != nil || replay {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE inbound_templates SET compatibility_state='unverified',compatibility_reason=NULL,last_validated_at=NULL,
            revision=revision+1,updated_at=? WHERE id=? AND revision=? AND archived_at IS NULL`, millis(audit.OccurredAt), id.String(), expected)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return &domain.ConflictError{Message: "inbound template changed since the page was loaded"}
		}
		if err := tx.SaveCommand(ctx, command); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, audit)
	})
	return replay, err
}

// InvalidateStaleCapabilityEvidence 把「兼容结论所依据的启动纪元与当前不符」的模板原子地置回待验证。
//
// 能力门禁验证的是「这个正在运行的 Xray 进程」；节点重启后配置可能已经变了（例如 policy 被去掉），
// 旧的 compatible 结论对新进程不成立，必须重跑门禁（FR-005/FR-029）。返回被置回的模板标识。
func (s *Store) InvalidateStaleCapabilityEvidence(ctx context.Context, currentEpoch string, now time.Time) ([]domain.ID, error) {
	if currentEpoch == "" {
		return nil, nil // 纪元未知时不做判断，避免把好模板误判为过期
	}
	var stale []domain.ID
	err := s.WithWriteTx(ctx, func(write ports.WriteTx) error {
		tx := write.(*txStore)
		rows, err := tx.tx.QueryContext(ctx, `SELECT id FROM inbound_templates
            WHERE archived_at IS NULL AND compatibility_state='compatible' AND COALESCE(validated_boot_epoch,'')<>?`, currentEpoch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			stale = append(stale, domain.ID(id))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(stale) == 0 {
			return nil
		}
		_, err = tx.tx.ExecContext(ctx, `UPDATE inbound_templates SET compatibility_state='unverified',
            compatibility_reason=?,last_validated_at=NULL,validated_boot_epoch=NULL,revision=revision+1,updated_at=?
            WHERE archived_at IS NULL AND compatibility_state='compatible' AND COALESCE(validated_boot_epoch,'')<>?`,
			"node restarted; capability gate must run again on the current Xray process", millis(now), currentEpoch)
		return err
	})
	return stale, err
}
