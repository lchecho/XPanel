package application

import (
	"bytes"
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
	TemplateID  domain.ID
	// Port 为空表示由面板自动分配池内最小空闲端口；非空时必须在池内且未被占用。
	Port        *int
	LimitBytes  *int64
	ResetDay    int
	RequestID   domain.ID
	ActorID     domain.ID
	Fingerprint []byte
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
	template, err := s.store.Template(ctx, input.TemplateID)
	if err != nil {
		return "", false, err
	}
	if template.Template.Compatibility != domain.CompatibilityCompatible || template.Template.ArchivedAt != nil {
		return "", false, &domain.InvalidStateError{Message: "inbound template is not compatible"}
	}
	// 端口分配：未指定时按池内升序取最小空闲端口；指定端口必须在池内且未被占用（FR-007/FR-009）。
	// 这里的检查是快速失败路径，最终唯一性由 dedicated_inbounds 的部分唯一索引保证（FR-008）。
	assigned, err := s.store.AssignedPorts(ctx, template.Template.ID)
	if err != nil {
		return "", false, err
	}
	var port int
	if input.Port != nil {
		if err := domain.ValidateRequestedPort(template.Template.Pool, *input.Port, assigned); err != nil {
			return "", false, err
		}
		port = *input.Port
	} else if port, err = domain.NextAvailablePort(template.Template.Pool, assigned); err != nil {
		return "", false, err
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
	key, err := security.GenerateUserKey(template.Template.Method)
	if err != nil {
		return "", false, err
	}
	ciphertext, nonce, err := s.keyring.Encrypt([]byte(key.Reveal()), security.SecretAAD("access_credentials", allocationID.String(), "user_key", 1))
	if err != nil {
		return "", false, err
	}
	// 每条专属入站有自己的服务端密钥，由面板生成，管理员不再提供（FR-004/FR-011）。
	serverKey, err := security.GenerateUserKey(template.Template.Method)
	if err != nil {
		return "", false, err
	}
	serverCiphertext, serverNonce, err := s.keyring.Encrypt([]byte(serverKey.Reveal()),
		security.SecretAAD("dedicated_inbounds", allocationID.String(), "server_key", 1))
	if err != nil {
		return "", false, err
	}
	version := int64(1)
	allocation := domain.AccessAllocation{ID: allocationID, UserID: userID, TemplateID: template.Template.ID, IdentityID: identityID,
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
	operation := domain.NewSynchronizationOperation(operationID, allocationID, 1, true, &version, domain.SyncCreate, domain.InboundPhaseFor(true), now)
	completed := now
	actor := input.ActorID
	limit := "unlimited"
	if input.LimitBytes != nil {
		limit = strconv.FormatInt(*input.LimitBytes, 10)
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionUserCreated, user.NormalizedName, input.TemplateID.String(), limit, strconv.Itoa(input.ResetDay))
	}
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionUserCreated, TargetType: "user", TargetID: userID, RequestFingerprint: fingerprint,
		State: domain.CommandCompleted, ResultReference: "/users/" + userID.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "user", TargetID: userID, Action: domain.ActionUserCreated, Result: domain.AuditAccepted,
		CommandID: &input.RequestID, OperationID: &operationID, SafeSummary: "user created; Xray projection pending"}
	inbound := domain.DedicatedInbound{AllocationID: allocationID, TemplateID: template.Template.ID,
		InboundTag: domain.InboundTag(allocationID), ListenAddress: template.Template.ListenAddress, Port: port,
		DesiredPresent: true, CreatedAt: now, UpdatedAt: now}
	portAuditID, err := domain.NewID()
	if err != nil {
		return "", false, err
	}
	// 端口分配留痕：摘要含端口与入站标签，不含任何密钥（FR-036/FR-037）。
	portAudit := domain.AuditEvent{ID: portAuditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "user", TargetID: userID, Action: domain.ActionPortAssigned, Result: domain.AuditAccepted,
		CommandID: &input.RequestID, OperationID: &operationID,
		SafeSummary: fmt.Sprintf("port %d assigned on %s (tag %s)", port, template.Template.ListenAddress, inbound.InboundTag)}
	record := ports.UserCreateRecord{User: user,
		Identity: domain.XrayUserIdentity{ID: identityID, InstanceID: template.Template.InstanceID, TemplateID: template.Template.ID,
			StatisticsID: statisticsID, Kind: domain.IdentityManaged, CreatedAt: now}, Allocation: allocation,
		Inbound: ports.InboundRecord{Inbound: inbound, ServerKeyCiphertext: serverCiphertext, ServerKeyNonce: serverNonce,
			KeyEncryptionVersion: 1}, Credential: credential,
		Policy: ports.QuotaPolicyRecord{AllocationID: allocationID, LimitBytes: input.LimitBytes, ResetDay: input.ResetDay, CreatedAt: now, UpdatedAt: now},
		Cycle: ports.QuotaCycleRecord{ID: cycleID, AllocationID: allocationID, StartsAt: start, EndsAt: end,
			Timezone: settings.QuotaTimezone, ResetDay: input.ResetDay, Status: "open", OpenedAt: now},
		Operation: operation, Command: command, Audit: audit, PortAudit: portAudit}
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
	Fingerprint      []byte
}

// UpdateUser 按 data-model §Atomic Transaction Boundaries 第 2 条提交编辑。配额状态与是否需要同步操作由 Store 在写事务内
// 用最新事实（策略、启用意图、生命周期、open 周期用量、revision）通过 domain.DecideTransition 决定，避免事务外旧快照覆盖较新意图。
// 调低配额至不高于当前用量立即封禁；调高或改为无限制且配额是唯一阻断原因时立即恢复（spec FR-009/FR-010）。
func (s *UserService) UpdateUser(ctx context.Context, input UpdateUserInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	if err := domain.ValidateQuotaPolicy(input.LimitBytes, input.ResetDay); err != nil {
		return false, err
	}
	normalized, err := domain.NormalizeDisplayName(input.DisplayName)
	if err != nil {
		return false, &domain.ValidationError{Field: "display_name", Message: err.Error()}
	}
	limit := "unlimited"
	if input.LimitBytes != nil {
		limit = strconv.FormatInt(*input.LimitBytes, 10)
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionUserUpdated, input.ID.String(), normalized, limit,
			strconv.Itoa(input.ResetDay), strconv.FormatBool(input.AdminEnabled), strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	if replayed, err := s.commandReplayed(ctx, input.RequestID, fingerprint); err != nil || replayed {
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
	ids := make([]domain.ID, 0, 4)
	for i := 0; i < 4; i++ {
		id, err := domain.NewID()
		if err != nil {
			return false, err
		}
		ids = append(ids, id)
	}
	actor := input.ActorID
	completed := now
	update := ports.UserUpdateRecord{UserID: record.User.ID, ExpectedRevision: input.ExpectedRevision, DisplayName: renamed.DisplayName,
		NormalizedName: renamed.NormalizedName, LimitBytes: input.LimitBytes, ResetDay: input.ResetDay, AdminEnabled: input.AdminEnabled, Now: now,
		OperationTemplate: domain.SynchronizationOperation{ID: ids[0], CreatedAt: now},
		QuotaAudit: &domain.AuditEvent{ID: ids[1], OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "user",
			TargetID: record.User.ID, Result: domain.AuditAccepted, CommandID: &input.RequestID},
		Command: domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionUserUpdated,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: fingerprint,
			State: domain.CommandCompleted, ResultReference: "/users/" + record.User.ID.String(), CreatedAt: now, CompletedAt: &completed}}
	intents := []struct {
		action, summary string
		when            bool
	}{
		{domain.ActionUserUpdated, "user profile or quota policy updated", true},
		{domain.ActionUserDisabled, "administrator disabled access", record.Allocation.AdminEnabled && !input.AdminEnabled},
		{domain.ActionUserEnabled, "administrator enabled access", !record.Allocation.AdminEnabled && input.AdminEnabled},
	}
	next := 2
	for _, item := range intents {
		if !item.when {
			continue
		}
		update.Audits = append(update.Audits, domain.AuditEvent{ID: ids[next], OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
			TargetType: "user", TargetID: record.User.ID, Action: item.action, Result: domain.AuditAccepted, CommandID: &input.RequestID, SafeSummary: item.summary})
		next++
	}
	replay, created, err := s.store.UpdateUser(ctx, update)
	if err != nil {
		return false, err
	}
	if !replay && created && s.notify != nil {
		s.notify()
	}
	return replay, nil
}

// ResetTrafficInput 描述手动重置本周期流量。
type ResetTrafficInput struct {
	ID          domain.ID
	RequestID   domain.ID
	ActorID     domain.ID
	Fingerprint []byte
}

// ResetTraffic 清零当前周期 accounted 用量；是否恢复由事务内最新事实决定（配额超限是唯一阻断原因时立即恢复，spec FR-032）。
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
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionTrafficReset, record.User.ID.String(), record.Cycle.ID.String())
	}
	if replayed, err := s.commandReplayed(ctx, input.RequestID, fingerprint); err != nil || replayed {
		return replayed, err
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
	opID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	reset := ports.QuotaResetRecord{AllocationID: record.Allocation.ID, CycleID: record.Cycle.ID, EventID: eventID, ActorID: input.ActorID, Now: now,
		OperationTemplate: domain.SynchronizationOperation{ID: opID, CreatedAt: now},
		Command: domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionTrafficReset,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: fingerprint,
			State: domain.CommandCompleted, ResultReference: "/users/" + record.User.ID.String(), CreatedAt: now, CompletedAt: &completed},
		Audit: domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "user",
			TargetID: record.User.ID, Action: domain.ActionTrafficReset, Result: domain.AuditAccepted, CommandID: &input.RequestID,
			SafeSummary: "current cycle accounted usage reset to zero"}}
	replay, created, err := s.store.ResetCycleTraffic(ctx, reset)
	if err != nil {
		return false, err
	}
	if !replay && created && s.notify != nil {
		s.notify()
	}
	return replay, nil
}

// commandReplayed 在版本校验之前识别重复提交：同一 _request_id 且同一载荷直接返回原结果，
// 同一 _request_id 但载荷不同返回冲突（http.md §General Rules）。
func (s *UserService) commandReplayed(ctx context.Context, requestID domain.ID, fingerprint []byte) (bool, error) {
	var existing *domain.DomainCommand
	err := s.store.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var inner error
		existing, inner = tx.FindCommand(ctx, requestID)
		return inner
	})
	if err != nil || existing == nil {
		return false, err
	}
	if !bytes.Equal(existing.RequestFingerprint, fingerprint) {
		return false, &domain.ConflictError{Message: "request identifier was reused with different input"}
	}
	return true, nil
}

// SetEnabledInput 描述启用/禁用按钮提交。
type SetEnabledInput struct {
	ID               domain.ID
	Enabled          bool
	ExpectedRevision domain.Revision
	RequestID        domain.ID
	ActorID          domain.ID
	Fingerprint      []byte
}

// SetAdminEnabled 只切换管理员启用意图，其余字段沿用当前值；重新启用先检查配额（spec FR-010）。
func (s *UserService) SetAdminEnabled(ctx context.Context, input SetEnabledInput) (bool, error) {
	record, err := s.store.User(ctx, input.ID)
	if err != nil {
		return false, err
	}
	return s.UpdateUser(ctx, UpdateUserInput{ID: input.ID, DisplayName: record.User.DisplayName, LimitBytes: record.Policy.LimitBytes,
		ResetDay: record.Policy.ResetDay, AdminEnabled: input.Enabled, ExpectedRevision: input.ExpectedRevision,
		RequestID: input.RequestID, ActorID: input.ActorID, Fingerprint: input.Fingerprint})
}

// LifecycleInput 描述轮换与删除提交。
type LifecycleInput struct {
	ID               domain.ID
	ExpectedRevision domain.Revision
	RequestID        domain.ID
	ActorID          domain.ID
	Fingerprint      []byte
}

// RotateCredential 生成下一版本密钥并启动 remove_old → add_desired → confirm 三阶段轮换（spec FR-011）。
// 轮换进行中再次轮换返回冲突；旧凭证在确认事务中销毁。
func (s *UserService) RotateCredential(ctx context.Context, input LifecycleInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionCredentialRotated, input.ID.String(), strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	if replayed, err := s.commandReplayed(ctx, input.RequestID, fingerprint); err != nil || replayed {
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
	if record.Credential.State == domain.CredentialPending {
		return false, &domain.ConflictError{Message: "credential rotation already in progress"}
	}
	now := s.clock.Now().UTC()
	credentialID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	key, err := security.GenerateUserKey(record.Template.Template.Method)
	if err != nil {
		return false, err
	}
	version := record.Credential.Version + 1
	ciphertext, nonce, err := s.keyring.Encrypt([]byte(key.Reveal()), security.SecretAAD("access_credentials", record.Allocation.ID.String(), "user_key", 1))
	if err != nil {
		return false, err
	}
	opID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	present := record.Allocation.DesiredPresent(record.User)
	actor := input.ActorID
	completed := now
	rotation := ports.RotationRecord{UserID: record.User.ID, AllocationID: record.Allocation.ID, ExpectedRevision: input.ExpectedRevision, Now: now,
		Credential: domain.AccessCredential{ID: credentialID, AllocationID: record.Allocation.ID, Version: version, State: domain.CredentialPending,
			KeyCiphertext: ciphertext, KeyNonce: nonce, KeyEncryptionVersion: 1, CreatedAt: now},
		Operation: domain.NewSynchronizationOperation(opID, record.Allocation.ID, record.Allocation.DesiredRevision+1, present, &version,
			domain.SyncRotate, domain.SyncRemoveOld, now),
		Command: domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionCredentialRotated,
			TargetType: "user", TargetID: record.User.ID,
			RequestFingerprint: fingerprint,
			State:              domain.CommandCompleted, ResultReference: "/users/" + record.User.ID.String(), CreatedAt: now, CompletedAt: &completed},
		Audit: domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "user",
			TargetID: record.User.ID, Action: domain.ActionCredentialRotated, Result: domain.AuditAccepted, CommandID: &input.RequestID,
			OperationID: &opID, SafeSummary: "credential rotation started; old credential retires after Xray confirms the new one"}}
	replay, err := s.store.RotateCredential(ctx, rotation)
	if err != nil {
		return false, err
	}
	if !replay && s.notify != nil {
		s.notify()
	}
	return replay, nil
}

// DeleteUser 软删除用户：保留流量历史与审计，移除确认后销毁全部密钥（spec FR-012）。
func (s *UserService) DeleteUser(ctx context.Context, input LifecycleInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionUserDeleted, input.ID.String(), strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	if replayed, err := s.commandReplayed(ctx, input.RequestID, fingerprint); err != nil || replayed {
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
	opID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	actor := input.ActorID
	completed := now
	deletion := ports.DeleteRecord{UserID: record.User.ID, AllocationID: record.Allocation.ID, ExpectedRevision: input.ExpectedRevision, Now: now,
		Operation: domain.NewSynchronizationOperation(opID, record.Allocation.ID, record.Allocation.DesiredRevision+1, false, nil, domain.SyncDelete, domain.InboundPhaseFor(false), now),
		Command: domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionUserDeleted,
			TargetType: "user", TargetID: record.User.ID,
			RequestFingerprint: fingerprint,
			State:              domain.CommandCompleted, ResultReference: "/users", CreatedAt: now, CompletedAt: &completed},
		Audit: domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "user",
			TargetID: record.User.ID, Action: domain.ActionUserDeleted, Result: domain.AuditAccepted, CommandID: &input.RequestID, OperationID: &opID,
			SafeSummary: "user soft-deleted; removal from Xray requested; history retained"}}
	replay, err := s.store.SoftDeleteUser(ctx, deletion)
	if err != nil {
		return false, err
	}
	if !replay && s.notify != nil {
		s.notify()
	}
	return replay, nil
}
