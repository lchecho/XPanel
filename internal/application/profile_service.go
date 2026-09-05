package application

import (
	"context"
	"errors"
	"strconv"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// ErrValidationStale 表示验证期间 profile 已被编辑：旧结果被丢弃，最新 revision 会重新验证。
var ErrValidationStale = errors.New("profile changed during validation; result discarded")

type ProfileInput struct {
	Name                  string
	InboundTag            string
	PublicHost            string
	PublicPort            int
	Method                string
	Network               domain.Network
	ServerKey             string
	BootstrapStatisticsID string
	RequestID             domain.ID
	ExpectedRevision      domain.Revision
	ActorID               domain.ID
	// Fingerprint 由 handler 以 session+action+target+规范化载荷生成；为空时由服务按业务字段计算。
	Fingerprint []byte
}

// RevalidateInput 描述手动重新验证：需要当前 revision，幂等排队。
type RevalidateInput struct {
	ID               domain.ID
	ExpectedRevision domain.Revision
	RequestID        domain.ID
	ActorID          domain.ID
	Fingerprint      []byte
}

// 核心函数：ProfileService 管理访问配置的登记、编辑、验证与重新验证。
//
// 职责：校验并持久化 profile 元数据与加密的服务端密钥；把验证结果按 profile revision 条件提交；不负责调度验证时机。
// 约束：验证结果、实例健康、bootstrap 身份与验证审计在一个事务中提交；验证期间发生编辑时丢弃旧结果（FR-021）。
type ProfileService struct {
	store   ports.Store
	adapter ports.Adapter
	keyring *security.Keyring
	clock   ports.Clock
	target  ports.InstanceTarget
	notify  func(domain.ID)
}

func NewProfileService(store ports.Store, adapter ports.Adapter, keyring *security.Keyring, clock ports.Clock,
	target ports.InstanceTarget, notify func(domain.ID)) *ProfileService {
	return &ProfileService{store: store, adapter: adapter, keyring: keyring, clock: clock, target: target, notify: notify}
}

func (s *ProfileService) RegisterProfile(ctx context.Context, input ProfileInput) (domain.ID, error) {
	if !input.RequestID.Valid() {
		return "", &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	if err := security.ValidateUserKey(input.Method, input.ServerKey); err != nil {
		return "", &domain.ValidationError{Field: "server_key", Message: "server key does not match the selected method"}
	}
	instance, err := s.store.ManagedInstance(ctx)
	if err != nil {
		return "", err
	}
	profileID, err := domain.NewID()
	if err != nil {
		return "", err
	}
	now := s.clock.Now().UTC()
	profile, err := domain.NewAccessProfile(profileID, instance.ID, input.Name, input.InboundTag, input.PublicHost,
		input.PublicPort, input.Method, input.Network, input.BootstrapStatisticsID, now)
	if err != nil {
		return "", err
	}
	ciphertext, nonce, err := s.keyring.Encrypt([]byte(input.ServerKey), security.SecretAAD("access_profiles", profileID.String(), "server_key", 1))
	if err != nil {
		return "", err
	}
	// 登记的指纹不得依赖随机生成的新 profile ID，否则同请求重放永远无法命中（FR-021）。
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionProfileRegistered, input.Name, input.InboundTag, input.PublicHost,
			strconv.Itoa(input.PublicPort), input.Method, string(input.Network), input.BootstrapStatisticsID, input.ServerKey)
	}
	command, audit, err := profileCommand(input, profileID, domain.ActionProfileRegistered, now, fingerprint)
	if err != nil {
		return "", err
	}
	record := ports.ProfileRecord{Profile: profile, ServerKeyCiphertext: ciphertext, ServerKeyNonce: nonce, KeyEncryptionVersion: 1}
	id, replay, err := s.store.CreateProfile(ctx, record, command, audit)
	if err != nil {
		return "", err
	}
	if !replay && s.notify != nil {
		s.notify(id)
	}
	return id, nil
}

// List 返回未归档的访问配置；compatibleOnly 时只返回可作为新用户目标的配置。
func (s *ProfileService) List(ctx context.Context, compatibleOnly bool) ([]ports.ProfileRecord, error) {
	return s.store.Profiles(ctx, compatibleOnly)
}

func (s *ProfileService) Get(ctx context.Context, id domain.ID) (ports.ProfileRecord, error) {
	return s.store.Profile(ctx, id)
}

func (s *ProfileService) UpdateProfile(ctx context.Context, id domain.ID, input ProfileInput) error {
	current, err := s.store.Profile(ctx, id)
	if err != nil {
		return err
	}
	validated, err := domain.NewAccessProfile(id, current.Profile.InstanceID, input.Name, input.InboundTag, input.PublicHost,
		input.PublicPort, input.Method, input.Network, input.BootstrapStatisticsID, s.clock.Now())
	if err != nil {
		return err
	}
	validated.Revision = current.Profile.Revision
	validated.CreatedAt = current.Profile.CreatedAt
	validated.UpdatedAt = s.clock.Now().UTC()
	validated.Compatibility = current.Profile.Compatibility
	validated.CompatibilityReason = current.Profile.CompatibilityReason
	validated.LastValidatedAt = current.Profile.LastValidatedAt
	validated.ArchivedAt = current.Profile.ArchivedAt
	record := ports.ProfileRecord{Profile: validated, ServerKeyCiphertext: current.ServerKeyCiphertext,
		ServerKeyNonce: current.ServerKeyNonce, KeyEncryptionVersion: current.KeyEncryptionVersion}
	contractChanged := current.Profile.InboundTag != validated.InboundTag || current.Profile.Method != validated.Method ||
		current.Profile.BootstrapStatisticsID != validated.BootstrapStatisticsID
	if input.ServerKey != "" {
		if err := security.ValidateUserKey(input.Method, input.ServerKey); err != nil {
			return &domain.ValidationError{Field: "server_key", Message: "server key does not match the selected method"}
		}
		record.ServerKeyCiphertext, record.ServerKeyNonce, err = s.keyring.Encrypt([]byte(input.ServerKey),
			security.SecretAAD("access_profiles", id.String(), "server_key", 1))
		if err != nil {
			return err
		}
		record.KeyEncryptionVersion = 1
		contractChanged = true
	}
	if contractChanged {
		record.Profile.Compatibility = domain.CompatibilityUnverified
		record.Profile.CompatibilityReason = ""
		record.Profile.LastValidatedAt = nil
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionProfileUpdated, id.String(), input.Name, input.InboundTag, input.PublicHost,
			strconv.Itoa(input.PublicPort), input.Method, string(input.Network), input.BootstrapStatisticsID, input.ServerKey,
			strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	command, audit, err := profileCommand(input, id, domain.ActionProfileUpdated, record.Profile.UpdatedAt, fingerprint)
	if err != nil {
		return err
	}
	if err := s.store.UpdateProfile(ctx, record, ports.RevisionMatch{Expected: input.ExpectedRevision}, command, audit); err != nil {
		return err
	}
	if contractChanged && s.notify != nil {
		s.notify(id)
	}
	return nil
}

func profileCommand(input ProfileInput, id domain.ID, action string, now time.Time, fingerprint []byte) (domain.DomainCommand, domain.AuditEvent, error) {
	auditID, err := domain.NewID()
	if err != nil {
		return domain.DomainCommand{}, domain.AuditEvent{}, err
	}
	actor := input.ActorID
	completed := now
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: action, TargetType: "profile", TargetID: id, RequestFingerprint: fingerprint,
		State: domain.CommandCompleted, ResultReference: "/profiles/" + id.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "profile", TargetID: id, Action: action, Result: domain.AuditSucceeded,
		CommandID: &input.RequestID, SafeSummary: "profile metadata saved"}
	return command, audit, nil
}

// Revalidate 以 revision 条件把 profile 置回 unverified 并递增 revision，使验证期间到达的旧结果被丢弃；同请求重放幂等。
func (s *ProfileService) Revalidate(ctx context.Context, input RevalidateInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionProfileRevalidationRequested, input.ID.String(), strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	now := s.clock.Now().UTC()
	actor := input.ActorID
	completed := now
	auditID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: domain.ActionProfileRevalidationRequested, TargetType: "profile", TargetID: input.ID, RequestFingerprint: fingerprint,
		State: domain.CommandCompleted, ResultReference: "/profiles/" + input.ID.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "profile",
		TargetID: input.ID, Action: domain.ActionProfileRevalidationRequested, Result: domain.AuditAccepted, CommandID: &input.RequestID,
		SafeSummary: "administrator requested profile revalidation"}
	replay, err := s.store.RequestRevalidation(ctx, input.ID, input.ExpectedRevision, command, audit)
	if err != nil {
		return false, err
	}
	if !replay && s.notify != nil {
		s.notify(input.ID)
	}
	return replay, nil
}

// RunValidation 探测实例并验证 profile 能力；结果只在 profile revision 未变化时提交，否则返回 ErrValidationStale。
func (s *ProfileService) RunValidation(ctx context.Context, id domain.ID) error {
	record, err := s.store.Profile(ctx, id)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	observation, err := s.adapter.Probe(ctx, s.target)
	if err != nil {
		return s.complete(ctx, record, domain.CompatibilityUnreachable, adapterSummary(err, "Xray API is unavailable"), nil, nil, now)
	}
	serverKey, err := s.keyring.Decrypt(record.ServerKeyCiphertext, record.ServerKeyNonce,
		security.SecretAAD("access_profiles", id.String(), "server_key", record.KeyEncryptionVersion))
	if err != nil {
		return s.complete(ctx, record, domain.CompatibilityIncompatible, "profile key could not be decrypted", &observation, nil, now)
	}
	capabilities, err := s.adapter.ValidateProfile(ctx, ports.RuntimeProfile{ID: id, InboundTag: record.Profile.InboundTag,
		Method: record.Profile.Method, BootstrapStatisticsID: record.Profile.BootstrapStatisticsID,
		ServiceKey: security.NewRedactedString(string(serverKey))})
	if err != nil {
		state := domain.CompatibilityIncompatible
		var adapterErr *ports.AdapterError
		if errors.As(err, &adapterErr) && adapterErr.Retryable {
			state = domain.CompatibilityUnreachable
		}
		return s.complete(ctx, record, state, adapterSummary(err, "profile validation failed"), &observation, nil, now)
	}
	if !capabilities.Compatible() {
		reason := capabilities.CompatibilityReason
		if reason == "" {
			reason = "profile does not satisfy the SS2022 multi-user contract"
		}
		return s.complete(ctx, record, domain.CompatibilityIncompatible, reason, &observation, nil, now)
	}
	identityID, err := domain.NewID()
	if err != nil {
		return err
	}
	bootstrap := &domain.XrayUserIdentity{ID: identityID, InstanceID: record.Profile.InstanceID, ProfileID: id,
		StatisticsID: record.Profile.BootstrapStatisticsID, Kind: domain.IdentityBootstrap, CreatedAt: now}
	return s.complete(ctx, record, domain.CompatibilityCompatible, "", &observation, bootstrap, now)
}

func adapterSummary(err error, fallback string) string {
	var adapterErr *ports.AdapterError
	if errors.As(err, &adapterErr) && adapterErr.SafeSummary != "" {
		return adapterErr.SafeSummary
	}
	return fallback
}

// complete 组装验证结果并按 profile revision 条件在一个事务中提交 compatibility、实例健康、bootstrap 身份与审计。
func (s *ProfileService) complete(ctx context.Context, record ports.ProfileRecord, state domain.CompatibilityState, reason string,
	observation *ports.InstanceObservation, bootstrap *domain.XrayUserIdentity, now time.Time) error {
	outcome := ports.ValidationOutcome{ProfileID: record.Profile.ID, ExpectedRevision: record.Profile.Revision, State: state, Reason: reason,
		ValidatedAt: now, Bootstrap: bootstrap, Health: "incompatible", ErrorCode: "incompatible_profile", ErrorSummary: reason}
	if observation != nil && observation.BootEpochKnown {
		outcome.BootEpoch = observation.BootEpoch.UTC().Format(time.RFC3339)
	}
	switch state {
	case domain.CompatibilityCompatible:
		success := now
		outcome.Health, outcome.ErrorCode, outcome.ErrorSummary, outcome.SuccessAt = "healthy", "", "", &success
	case domain.CompatibilityUnreachable:
		outcome.Health, outcome.ErrorCode = "unreachable", "instance_unavailable"
	}
	auditID, err := domain.NewID()
	if err != nil {
		return err
	}
	result := domain.AuditSucceeded
	if state != domain.CompatibilityCompatible {
		result = domain.AuditFailed
	}
	outcome.Audit = domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "profile",
		TargetID: record.Profile.ID, Action: domain.ActionProfileValidated, Result: result, SafeSummary: reason}
	applied, err := s.store.CompleteProfileValidation(ctx, outcome)
	if err != nil {
		return err
	}
	if !applied {
		return ErrValidationStale
	}
	return nil
}
