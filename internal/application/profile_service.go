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
}

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
	command, audit, err := profileCommand(input, profileID, domain.ActionProfileRegistered, now)
	if err != nil {
		return "", err
	}
	record := ports.ProfileRecord{Profile: profile, ServerKeyCiphertext: ciphertext, ServerKeyNonce: nonce, KeyEncryptionVersion: 1}
	if err := s.store.CreateProfile(ctx, record, command, audit); err != nil {
		return "", err
	}
	if s.notify != nil {
		s.notify(profileID)
	}
	return profileID, nil
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
	command, audit, err := profileCommand(input, id, domain.ActionProfileUpdated, record.Profile.UpdatedAt)
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

func profileCommand(input ProfileInput, id domain.ID, action string, now time.Time) (domain.DomainCommand, domain.AuditEvent, error) {
	auditID, err := domain.NewID()
	if err != nil {
		return domain.DomainCommand{}, domain.AuditEvent{}, err
	}
	actor := input.ActorID
	completed := now
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor,
		CommandType: action, TargetType: "profile", TargetID: id,
		RequestFingerprint: domain.Fingerprint(action, id.String(), input.Name, input.InboundTag, input.PublicHost,
			strconv.Itoa(input.PublicPort), input.Method, string(input.Network), input.BootstrapStatisticsID, input.ServerKey),
		State: domain.CommandCompleted, ResultReference: "/profiles/" + id.String(), CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
		TargetType: "profile", TargetID: id, Action: action, Result: domain.AuditSucceeded,
		CommandID: &input.RequestID, SafeSummary: "profile metadata saved"}
	return command, audit, nil
}

func (s *ProfileService) Revalidate(ctx context.Context, id domain.ID) error {
	if err := s.store.SetProfileCompatibility(ctx, id, domain.CompatibilityUnverified, "", s.clock.Now()); err != nil {
		return err
	}
	if s.notify != nil {
		s.notify(id)
	}
	return nil
}

func (s *ProfileService) RunValidation(ctx context.Context, id domain.ID) error {
	record, err := s.store.Profile(ctx, id)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	observation, err := s.adapter.Probe(ctx, s.target)
	if err != nil {
		return s.finishValidation(ctx, record, domain.CompatibilityUnreachable, adapterSummary(err, "Xray API is unavailable"), nil, now)
	}
	serverKey, err := s.keyring.Decrypt(record.ServerKeyCiphertext, record.ServerKeyNonce,
		security.SecretAAD("access_profiles", id.String(), "server_key", record.KeyEncryptionVersion))
	if err != nil {
		return s.finishValidation(ctx, record, domain.CompatibilityIncompatible, "profile key could not be decrypted", &observation, now)
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
		return s.finishValidation(ctx, record, state, adapterSummary(err, "profile validation failed"), &observation, now)
	}
	if !capabilities.Compatible() {
		reason := capabilities.CompatibilityReason
		if reason == "" {
			reason = "profile does not satisfy the SS2022 multi-user contract"
		}
		return s.finishValidation(ctx, record, domain.CompatibilityIncompatible, reason, &observation, now)
	}
	identityID, err := domain.NewID()
	if err != nil {
		return err
	}
	if err := s.store.RegisterBootstrapIdentity(ctx, domain.XrayUserIdentity{ID: identityID, InstanceID: record.Profile.InstanceID,
		ProfileID: id, StatisticsID: record.Profile.BootstrapStatisticsID, Kind: domain.IdentityBootstrap, CreatedAt: now}); err != nil {
		return err
	}
	return s.finishValidation(ctx, record, domain.CompatibilityCompatible, "", &observation, now)
}

func adapterSummary(err error, fallback string) string {
	var adapterErr *ports.AdapterError
	if errors.As(err, &adapterErr) && adapterErr.SafeSummary != "" {
		return adapterErr.SafeSummary
	}
	return fallback
}

func (s *ProfileService) finishValidation(ctx context.Context, record ports.ProfileRecord, state domain.CompatibilityState,
	reason string, observation *ports.InstanceObservation, now time.Time) error {
	if err := s.store.SetProfileCompatibility(ctx, record.Profile.ID, state, reason, now); err != nil {
		return err
	}
	health, boot, code := "incompatible", "", "incompatible_profile"
	var success *time.Time
	if observation != nil {
		boot = observation.BootEpoch.UTC().Format(time.RFC3339)
	}
	if state == domain.CompatibilityCompatible {
		health, code, reason, success = "healthy", "", "", &now
	} else if state == domain.CompatibilityUnreachable {
		health, code = "unreachable", "instance_unavailable"
	}
	if err := s.store.SetInstanceHealth(ctx, health, boot, code, success, reason, now); err != nil {
		return err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return err
	}
	result := domain.AuditSucceeded
	if state != domain.CompatibilityCompatible {
		result = domain.AuditFailed
	}
	return s.store.AppendAudit(ctx, domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorSystem,
		TargetType: "profile", TargetID: record.Profile.ID, Action: domain.ActionProfileValidated,
		Result: result, SafeSummary: reason})
}
