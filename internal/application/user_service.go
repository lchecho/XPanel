package application

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

type CreateUserInput struct {
	DisplayName string
	ProfileID   domain.ID
	LimitBytes  *int64
	ResetDay    int
	RequestID   domain.ID
	ActorID     domain.ID
}

type UserService struct {
	store   ports.Store
	keyring *security.Keyring
	clock   ports.Clock
	notify  func()
}

func NewUserService(store ports.Store, keyring *security.Keyring, clock ports.Clock, notify func()) *UserService {
	return &UserService{store: store, keyring: keyring, clock: clock, notify: notify}
}

func (s *UserService) CreateUser(ctx context.Context, input CreateUserInput) (domain.ID, bool, error) {
	if !input.RequestID.Valid() {
		return "", false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	if input.ResetDay < 1 || input.ResetDay > 28 {
		return "", false, &domain.ValidationError{Field: "reset_day", Message: "reset day must be between 1 and 28"}
	}
	if input.LimitBytes != nil && (*input.LimitBytes <= 0 || *input.LimitBytes > 1<<62) {
		return "", false, &domain.ValidationError{Field: "limit_bytes", Message: "quota must be positive and at most 2^62 bytes"}
	}
	profile, err := s.store.Profile(ctx, input.ProfileID)
	if err != nil {
		return "", false, err
	}
	if profile.Profile.Compatibility != domain.CompatibilityCompatible || profile.Profile.ArchivedAt != nil {
		return "", false, &domain.InvalidStateError{Message: "profile is not compatible"}
	}
	now := s.clock.Now().UTC()
	userID, allocationID, identityID, credentialID, cycleID, operationID, auditID := domain.ID(""), domain.ID(""), domain.ID(""), domain.ID(""), domain.ID(""), domain.ID(""), domain.ID("")
	for _, target := range []*domain.ID{&userID, &allocationID, &identityID, &credentialID, &cycleID, &operationID, &auditID} {
		*target, err = domain.NewID()
		if err != nil {
			return "", false, err
		}
	}
	user, err := domain.NewManagedUser(userID, input.DisplayName, now)
	if err != nil {
		return "", false, err
	}
	statisticsID, err := security.StatisticsID(allocationID.String())
	if err != nil {
		return "", false, err
	}
	key, err := security.GenerateUserKey(profile.Profile.Method)
	if err != nil {
		return "", false, err
	}
	ciphertext, nonce, err := s.keyring.Encrypt([]byte(key.Reveal()), security.SecretAAD("access_credentials", allocationID.String(), "user_key", 1))
	if err != nil {
		return "", false, err
	}
	version := int64(1)
	allocation := domain.AccessAllocation{ID: allocationID, UserID: userID, ProfileID: profile.Profile.ID, IdentityID: identityID,
		AdminEnabled: true, QuotaState: domain.QuotaWithinLimit, ProjectionState: domain.ProjectionPending,
		DesiredRevision: 1, DesiredCredentialVersion: version, CreatedAt: now, UpdatedAt: now}
	credential := domain.AccessCredential{ID: credentialID, AllocationID: allocationID, Version: version,
		State: domain.CredentialPending, KeyCiphertext: ciphertext, KeyNonce: nonce, KeyEncryptionVersion: 1, CreatedAt: now}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return "", false, err
	}
	start, end, err := initialCycleBounds(now, settings.QuotaTimezone, input.ResetDay)
	if err != nil {
		return "", false, &domain.ValidationError{Field: "reset_day", Message: "quota cycle could not be calculated"}
	}
	operation := domain.NewSynchronizationOperation(operationID, allocationID, 1, true, &version, domain.SyncCreate, domain.SyncAddDesired, now)
	completed := now
	actor := input.ActorID
	limit := "unlimited"
	if input.LimitBytes != nil {
		limit = strconv.FormatInt(*input.LimitBytes, 10)
	}
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionUserCreated, TargetType: "user", TargetID: userID,
		RequestFingerprint: domain.Fingerprint(domain.ActionUserCreated, user.NormalizedName, input.ProfileID.String(), limit, strconv.Itoa(input.ResetDay)),
		State:              domain.CommandCompleted, ResultReference: "/users/" + userID.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "user", TargetID: userID, Action: domain.ActionUserCreated, Result: domain.AuditAccepted,
		CommandID: &input.RequestID, OperationID: &operationID, SafeSummary: "user created; Xray projection pending"}
	record := ports.UserCreateRecord{User: user,
		Identity: domain.XrayUserIdentity{ID: identityID, InstanceID: profile.Profile.InstanceID, ProfileID: profile.Profile.ID,
			StatisticsID: statisticsID, Kind: domain.IdentityManaged, CreatedAt: now}, Allocation: allocation, Credential: credential,
		Policy: ports.QuotaPolicyRecord{AllocationID: allocationID, LimitBytes: input.LimitBytes, ResetDay: input.ResetDay, CreatedAt: now, UpdatedAt: now},
		Cycle: ports.QuotaCycleRecord{ID: cycleID, AllocationID: allocationID, StartsAt: start, EndsAt: end,
			Timezone: settings.QuotaTimezone, ResetDay: input.ResetDay, Status: "open", OpenedAt: now},
		Operation: operation, Command: command, Audit: audit}
	id, replay, err := s.store.CreateUser(ctx, record)
	if err != nil {
		return "", false, err
	}
	if !replay && s.notify != nil {
		s.notify()
	}
	return id, replay, nil
}

func initialCycleBounds(now time.Time, timezone string, resetDay int) (time.Time, time.Time, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), resetDay, 0, 0, 0, 0, location)
	if local.Before(start) {
		start = start.AddDate(0, -1, 0)
	}
	end := start.AddDate(0, 1, 0)
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid quota cycle")
	}
	return start.UTC(), end.UTC(), nil
}

func (s *UserService) User(ctx context.Context, id domain.ID) (ports.UserRecord, error) {
	return s.store.User(ctx, id)
}

// List 按名称与派生状态筛选用户；已删除用户仅在显式要求时返回。
func (s *UserService) List(ctx context.Context, filter ports.UserFilter) ([]ports.UserRecord, error) {
	if filter.Status == string(domain.DisplayDeleted) {
		filter.IncludeDeleted = true
	}
	return s.store.ListUsers(ctx, filter)
}

// UpdateUserInput 描述编辑页提交的显示名称、配额策略与启用意图。
type UpdateUserInput struct {
	ID               domain.ID
	DisplayName      string
	LimitBytes       *int64
	ResetDay         int
	AdminEnabled     bool
	ExpectedRevision domain.Revision
	RequestID        domain.ID
	ActorID          domain.ID
}

// UpdateUser 按 data-model §Atomic Transaction Boundaries 第 2 条提交编辑；期望存在状态变化时写入同步操作。
// 调低配额至不高于当前用量立即封禁；调高或改为无限制且配额是唯一阻断原因时立即恢复（spec FR-009/FR-010）。
func (s *UserService) UpdateUser(ctx context.Context, input UpdateUserInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	if err := domain.ValidateQuotaPolicy(input.LimitBytes, input.ResetDay); err != nil {
		return false, err
	}
	if replayed, err := s.commandReplayed(ctx, input.RequestID); err != nil || replayed {
		return replayed, err
	}
	record, err := s.store.User(ctx, input.ID)
	if err != nil {
		return false, err
	}
	if record.User.Lifecycle == domain.LifecycleDeleted {
		return false, &domain.InvalidStateError{Message: "user is deleted"}
	}
	if record.User.Revision != input.ExpectedRevision {
		return false, &domain.ConflictError{Message: "user changed since the page was loaded"}
	}
	now := s.clock.Now().UTC()
	renamed, err := domain.NewManagedUser(record.User.ID, input.DisplayName, now)
	if err != nil {
		return false, err
	}
	exceeded := domain.IsQuotaExceeded(input.LimitBytes, record.Cycle.AccountedUplinkBytes, record.Cycle.AccountedDownlinkBytes)
	next := record.Allocation
	next.AdminEnabled = input.AdminEnabled
	next.QuotaState = domain.QuotaWithinLimit
	if exceeded {
		next.QuotaState = domain.QuotaExceeded
	}
	wasPresent := record.Allocation.DesiredPresent(record.User)
	willBePresent := next.DesiredPresent(record.User)
	actor := input.ActorID
	limit := "unlimited"
	if input.LimitBytes != nil {
		limit = strconv.FormatInt(*input.LimitBytes, 10)
	}
	completed := now
	update := ports.UserUpdateRecord{UserID: record.User.ID, ExpectedRevision: input.ExpectedRevision, DisplayName: renamed.DisplayName,
		NormalizedName: renamed.NormalizedName, LimitBytes: input.LimitBytes, ResetDay: input.ResetDay, AdminEnabled: input.AdminEnabled,
		QuotaState: next.QuotaState, Now: now,
		Command: domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionUserUpdated,
			TargetType: "user", TargetID: record.User.ID,
			RequestFingerprint: domain.Fingerprint(domain.ActionUserUpdated, record.User.ID.String(), renamed.NormalizedName, limit,
				strconv.Itoa(input.ResetDay), strconv.FormatBool(input.AdminEnabled), strconv.FormatInt(int64(input.ExpectedRevision), 10)),
			State: domain.CommandCompleted, ResultReference: "/users/" + record.User.ID.String(), CreatedAt: now, CompletedAt: &completed}}
	var operationID *domain.ID
	if willBePresent != wasPresent {
		opID, err := domain.NewID()
		if err != nil {
			return false, err
		}
		reason, phase := domain.SyncDisable, domain.SyncRemoveOld
		switch {
		case !willBePresent && exceeded && record.Allocation.QuotaState == domain.QuotaWithinLimit && input.AdminEnabled:
			reason = domain.SyncQuotaBlock
		case willBePresent && record.Allocation.QuotaState == domain.QuotaExceeded && record.Allocation.AdminEnabled:
			reason, phase = domain.SyncQuotaRestore, domain.SyncAddDesired
		case willBePresent:
			reason, phase = domain.SyncEnable, domain.SyncAddDesired
		}
		version := record.Allocation.DesiredCredentialVersion
		op := domain.NewSynchronizationOperation(opID, record.Allocation.ID, record.Allocation.DesiredRevision+1, willBePresent, &version, reason, phase, now)
		update.Operation = &op
		operationID = &opID
	}
	audits := []struct {
		action, summary string
		when            bool
	}{
		{domain.ActionUserUpdated, "user profile or quota policy updated", true},
		{domain.ActionUserDisabled, "administrator disabled access", record.Allocation.AdminEnabled && !input.AdminEnabled},
		{domain.ActionUserEnabled, "administrator enabled access", !record.Allocation.AdminEnabled && input.AdminEnabled},
		{domain.ActionQuotaExceeded, "quota lowered to or below current usage; removal requested", exceeded && record.Allocation.QuotaState == domain.QuotaWithinLimit},
		{domain.ActionCycleRestored, "quota raised above current usage; access restore requested", !exceeded && record.Allocation.QuotaState == domain.QuotaExceeded},
	}
	for _, item := range audits {
		if !item.when {
			continue
		}
		auditID, err := domain.NewID()
		if err != nil {
			return false, err
		}
		update.Audits = append(update.Audits, domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
			TargetType: "user", TargetID: record.User.ID, Action: item.action, Result: domain.AuditAccepted, CommandID: &input.RequestID,
			OperationID: operationID, SafeSummary: item.summary})
	}
	replay, err := s.store.UpdateUser(ctx, update)
	if err != nil {
		return false, err
	}
	if !replay && update.Operation != nil && s.notify != nil {
		s.notify()
	}
	return replay, nil
}

// ResetTrafficInput 描述手动重置本周期流量。
type ResetTrafficInput struct {
	ID        domain.ID
	RequestID domain.ID
	ActorID   domain.ID
}

// ResetTraffic 清零当前周期 accounted 用量；配额超限是唯一阻断原因时立即请求恢复（spec FR-032）。
func (s *UserService) ResetTraffic(ctx context.Context, input ResetTrafficInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	record, err := s.store.User(ctx, input.ID)
	if err != nil {
		return false, err
	}
	if record.User.Lifecycle == domain.LifecycleDeleted {
		return false, &domain.InvalidStateError{Message: "user is deleted"}
	}
	now := s.clock.Now().UTC()
	actor := input.ActorID
	completed := now
	eventID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	reset := ports.QuotaResetRecord{AllocationID: record.Allocation.ID, CycleID: record.Cycle.ID, EventID: eventID, ActorID: input.ActorID, Now: now,
		Command: domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionTrafficReset,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: domain.Fingerprint(domain.ActionTrafficReset, record.User.ID.String(), record.Cycle.ID.String()),
			State: domain.CommandCompleted, ResultReference: "/users/" + record.User.ID.String(), CreatedAt: now, CompletedAt: &completed},
		Audit: domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "user",
			TargetID: record.User.ID, Action: domain.ActionTrafficReset, Result: domain.AuditAccepted, CommandID: &input.RequestID,
			SafeSummary: "current cycle accounted usage reset to zero"}}
	if record.Allocation.QuotaState == domain.QuotaExceeded && record.Allocation.AdminEnabled {
		opID, err := domain.NewID()
		if err != nil {
			return false, err
		}
		version := record.Allocation.DesiredCredentialVersion
		op := domain.NewSynchronizationOperation(opID, record.Allocation.ID, record.Allocation.DesiredRevision+1, true, &version,
			domain.SyncQuotaRestore, domain.SyncAddDesired, now)
		reset.Operation = &op
		reset.Audit.OperationID = &opID
	}
	replay, err := s.store.ResetCycleTraffic(ctx, reset)
	if err != nil {
		return false, err
	}
	if !replay && reset.Operation != nil && s.notify != nil {
		s.notify()
	}
	return replay, nil
}

// commandReplayed 在版本校验之前识别重复提交：同一 _request_id 直接返回原结果（http.md §General Rules）。
func (s *UserService) commandReplayed(ctx context.Context, requestID domain.ID) (bool, error) {
	var existing *domain.DomainCommand
	err := s.store.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var inner error
		existing, inner = tx.FindCommand(ctx, requestID)
		return inner
	})
	return existing != nil, err
}
