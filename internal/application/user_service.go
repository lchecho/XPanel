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
