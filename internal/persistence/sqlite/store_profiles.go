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

func (s *Store) RegisterBootstrapIdentity(ctx context.Context, identity domain.XrayUserIdentity) error {
	_, err := s.db.Write.ExecContext(ctx, `INSERT INTO xray_user_identities
        (id,instance_id,profile_id,statistics_id,kind,created_at) VALUES (?,?,?,?,?,?)
        ON CONFLICT(instance_id,statistics_id) DO NOTHING`, identity.ID.String(), identity.InstanceID.String(),
		identity.ProfileID.String(), identity.StatisticsID, identity.Kind, millis(identity.CreatedAt))
	return err
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
