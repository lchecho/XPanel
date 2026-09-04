package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
            (id,instance_id,profile_id,statistics_id,kind,created_at) VALUES (?,?,?,?,?,?)`, i.ID.String(), i.InstanceID.String(),
			i.ProfileID.String(), i.StatisticsID, i.Kind, millis(i.CreatedAt)); err != nil {
			return translateConstraint(err, "statistics identity already exists")
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO access_allocations
            (id,user_id,profile_id,identity_id,admin_enabled,quota_state,projection_state,observed_present,
             desired_revision,synced_revision,desired_credential_version,synced_credential_version,last_sync_at,
             last_sync_error_code,last_sync_error_summary,created_at,updated_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID.String(), a.UserID.String(), a.ProfileID.String(), a.IdentityID.String(),
			boolInt(a.AdminEnabled), a.QuotaState, a.ProjectionState, nullableBool(a.ObservedPresent), a.DesiredRevision,
			a.SyncedRevision, a.DesiredCredentialVersion, nullableInt64(a.SyncedCredentialVersion), nullTime(a.LastSyncAt),
			nullString(a.LastSyncErrorCode), nullString(a.LastSyncErrorSummary), millis(a.CreatedAt), millis(a.UpdatedAt)); err != nil {
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
    i.id,i.instance_id,i.profile_id,i.statistics_id,i.kind,i.created_at,
    a.id,a.user_id,a.profile_id,a.identity_id,a.admin_enabled,a.quota_state,a.projection_state,a.observed_present,
    a.desired_revision,a.synced_revision,a.desired_credential_version,a.synced_credential_version,a.last_sync_at,
    COALESCE(a.last_sync_error_code,''),COALESCE(a.last_sync_error_summary,''),a.created_at,a.updated_at,
    c.id,c.allocation_id,c.version,c.state,c.key_ciphertext,c.key_nonce,c.key_encryption_version,c.created_at,c.activated_at,c.retired_at,
    p.id,p.instance_id,p.name,p.normalized_name,p.inbound_tag,p.public_host,p.public_port,p.method,p.network,
    p.server_key_ciphertext,p.server_key_nonce,p.key_encryption_version,p.bootstrap_statistics_id,p.compatibility_state,
    COALESCE(p.compatibility_reason,''),p.last_validated_at,p.revision,p.archived_at,p.created_at,p.updated_at,
    qp.allocation_id,qp.limit_bytes,qp.reset_day,qp.revision,qp.created_at,qp.updated_at,
    qc.id,qc.allocation_id,qc.starts_at_utc,qc.ends_at_utc,qc.timezone_name,qc.reset_day,qc.status,
    qc.gross_uplink_bytes,qc.gross_downlink_bytes,qc.accounted_uplink_bytes,qc.accounted_downlink_bytes,
    qc.manual_reset_count,qc.opened_at,qc.closed_at
    FROM managed_users u
    JOIN access_allocations a ON a.user_id=u.id
    JOIN xray_user_identities i ON i.id=a.identity_id
    JOIN access_profiles p ON p.id=a.profile_id
    JOIN access_credentials c ON c.allocation_id=a.id AND c.version=a.desired_credential_version
    JOIN quota_policies qp ON qp.allocation_id=a.id
    JOIN quota_cycles qc ON qc.allocation_id=a.id AND qc.status='open'`

func scanUser(row scanner) (ports.UserRecord, error) {
	var record ports.UserRecord
	var userID, identityID, identityInstanceID, identityProfileID, allocationID, allocationUserID, allocationProfileID, allocationIdentityID string
	var credentialID, credentialAllocationID, profileID, profileInstanceID, policyAllocationID, cycleID, cycleAllocationID string
	var userCreated, userUpdated, identityCreated, allocationCreated, allocationUpdated, credentialCreated int64
	var profileCreated, profileUpdated, policyCreated, policyUpdated, starts, ends, opened int64
	var userDeleted, lastSync, credentialActivated, credentialRetired, profileValidated, profileArchived, cycleClosed sql.NullInt64
	var observed, syncedCredential, keyVersion, limit sql.NullInt64
	err := row.Scan(&userID, &record.User.DisplayName, &record.User.NormalizedName, &record.User.Lifecycle, &record.User.Revision,
		&userCreated, &userUpdated, &userDeleted, &identityID, &identityInstanceID, &identityProfileID,
		&record.Identity.StatisticsID, &record.Identity.Kind, &identityCreated, &allocationID, &allocationUserID,
		&allocationProfileID, &allocationIdentityID, &record.Allocation.AdminEnabled, &record.Allocation.QuotaState,
		&record.Allocation.ProjectionState, &observed, &record.Allocation.DesiredRevision, &record.Allocation.SyncedRevision,
		&record.Allocation.DesiredCredentialVersion, &syncedCredential, &lastSync, &record.Allocation.LastSyncErrorCode,
		&record.Allocation.LastSyncErrorSummary, &allocationCreated, &allocationUpdated, &credentialID, &credentialAllocationID,
		&record.Credential.Version, &record.Credential.State, &record.Credential.KeyCiphertext, &record.Credential.KeyNonce,
		&keyVersion, &credentialCreated, &credentialActivated, &credentialRetired, &profileID, &profileInstanceID,
		&record.Profile.Profile.Name, &record.Profile.Profile.NormalizedName, &record.Profile.Profile.InboundTag,
		&record.Profile.Profile.PublicHost, &record.Profile.Profile.PublicPort, &record.Profile.Profile.Method,
		&record.Profile.Profile.Network, &record.Profile.ServerKeyCiphertext, &record.Profile.ServerKeyNonce,
		&record.Profile.KeyEncryptionVersion, &record.Profile.Profile.BootstrapStatisticsID, &record.Profile.Profile.Compatibility,
		&record.Profile.Profile.CompatibilityReason, &profileValidated, &record.Profile.Profile.Revision, &profileArchived,
		&profileCreated, &profileUpdated, &policyAllocationID, &limit, &record.Policy.ResetDay, &record.Policy.Revision,
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
	record.Identity.ID, record.Identity.InstanceID, record.Identity.ProfileID = domain.ID(identityID), domain.ID(identityInstanceID), domain.ID(identityProfileID)
	record.Identity.CreatedAt = fromMillis(identityCreated)
	record.Allocation.ID, record.Allocation.UserID, record.Allocation.ProfileID, record.Allocation.IdentityID = domain.ID(allocationID), domain.ID(allocationUserID), domain.ID(allocationProfileID), domain.ID(allocationIdentityID)
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
	record.Profile.Profile.ID, record.Profile.Profile.InstanceID = domain.ID(profileID), domain.ID(profileInstanceID)
	record.Profile.Profile.CreatedAt, record.Profile.Profile.UpdatedAt = fromMillis(profileCreated), fromMillis(profileUpdated)
	setTime(&record.Profile.Profile.LastValidatedAt, profileValidated)
	setTime(&record.Profile.Profile.ArchivedAt, profileArchived)
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
func (s *Store) ListUsers(ctx context.Context, filter ports.UserFilter) ([]ports.UserRecord, error) {
	query := userSelect + ` WHERE (? = '' OR u.normalized_name LIKE ?) AND (? = 1 OR u.deleted_at IS NULL) ORDER BY u.normalized_name, u.id`
	normalized, _ := domain.NormalizeDisplayName(filter.Query)
	pattern := "%" + strings.ReplaceAll(strings.ReplaceAll(normalized, "%", ""), "_", "") + "%"
	if strings.TrimSpace(filter.Query) == "" {
		normalized = ""
	}
	rows, err := s.db.Read.QueryContext(ctx, query, normalized, pattern, boolInt(filter.IncludeDeleted))
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
		return state == domain.DisplayActive
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
