package application

import (
	"context"
	"strconv"
	"strings"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// 核心函数：SettingsService 读取与更新面板级设置（全局配额时区）。
type SettingsService struct {
	store ports.Store
	clock ports.Clock
}

func NewSettingsService(store ports.Store) *SettingsService {
	return &SettingsService{store: store, clock: ports.SystemClock{}}
}

func (s *SettingsService) WithClock(clock ports.Clock) *SettingsService {
	s.clock = clock
	return s
}

func (s *SettingsService) Get(ctx context.Context) (ports.PanelSettingsRecord, error) {
	return s.store.Settings(ctx)
}

// Location 返回面板配额时区；加载失败时退回 UTC，避免页面渲染中断。
func (s *SettingsService) Location(ctx context.Context) *time.Location {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return time.UTC
	}
	location, err := time.LoadLocation(settings.QuotaTimezone)
	if err != nil {
		return time.UTC
	}
	return location
}

type UpdateSettingsInput struct {
	QuotaTimezone    string
	ExpectedRevision domain.Revision
	RequestID        domain.ID
	ActorID          domain.ID
	Fingerprint      []byte
}

const settingsTargetID = domain.ID("00000000-0000-4000-8000-000000000001")

// Update 校验 IANA 时区并按版本更新；变更只从下一配额周期起生效。
func (s *SettingsService) Update(ctx context.Context, input UpdateSettingsInput) (bool, error) {
	if !input.RequestID.Valid() {
		return false, &domain.ValidationError{Field: "_request_id", Message: "invalid request identifier"}
	}
	name := strings.TrimSpace(input.QuotaTimezone)
	if name == "" || name == "Local" {
		return false, &domain.ValidationError{Field: "quota_timezone", Message: "timezone must be a valid IANA name"}
	}
	if _, err := time.LoadLocation(name); err != nil {
		return false, &domain.ValidationError{Field: "quota_timezone", Message: "timezone must be a valid IANA name"}
	}
	now := s.clock.Now().UTC()
	actor := input.ActorID
	completed := now
	auditID, err := domain.NewID()
	if err != nil {
		return false, err
	}
	fingerprint := input.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = domain.Fingerprint(domain.ActionSettingsUpdated, name, strconv.FormatInt(int64(input.ExpectedRevision), 10))
	}
	command := domain.DomainCommand{ID: input.RequestID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionSettingsUpdated,
		TargetType: "settings", TargetID: settingsTargetID, RequestFingerprint: fingerprint,
		State: domain.CommandCompleted, ResultReference: "/settings", CreatedAt: now, CompletedAt: &completed}
	audit := domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor, TargetType: "settings",
		TargetID: settingsTargetID, Action: domain.ActionSettingsUpdated, Result: domain.AuditSucceeded, CommandID: &input.RequestID,
		SafeSummary: "quota timezone set to " + name + "; applies from the next cycle"}
	return s.store.UpdateSettings(ctx, name, input.ExpectedRevision, command, audit)
}
