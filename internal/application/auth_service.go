package application

import (
	"context"
	"errors"
	"sync"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

var ErrInvalidCredentials = errors.New("invalid username or password")

type AuthService struct {
	store     ports.AuthStore
	clock     ports.Clock
	dummyHash string
	mu        sync.Mutex
	bySource  map[string][]time.Time
	global    []time.Time
}

func NewAuthService(store ports.AuthStore, clock ports.Clock) (*AuthService, error) {
	dummy, err := security.HashPassword([]byte("xpanel dummy password value"))
	if err != nil {
		return nil, err
	}
	return &AuthService{store: store, clock: clock, dummyHash: dummy, bySource: make(map[string][]time.Time)}, nil
}

func (s *AuthService) InitializeAdministrator(ctx context.Context, username string, password []byte) (domain.ID, error) {
	normalized, err := security.NormalizeUsername(username)
	if err != nil {
		return "", &domain.ValidationError{Field: "username", Message: err.Error()}
	}
	if err := security.ValidatePassword(normalized, password); err != nil {
		return "", &domain.ValidationError{Field: "password", Message: err.Error()}
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return "", err
	}
	id, err := domain.NewID()
	if err != nil {
		return "", err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return "", err
	}
	now := s.clock.Now()
	err = s.store.WithWriteTx(ctx, func(tx ports.WriteTx) error {
		existing, err := tx.Administrator(ctx)
		if err != nil {
			return err
		}
		if existing != nil {
			return &domain.ConflictError{Message: "administrator already initialized; use reset-password"}
		}
		if err := tx.CreateAdministrator(ctx, ports.AdministratorRecord{ID: id, Username: normalized, PasswordHash: hash,
			PasswordVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorLocalCLI,
			TargetType: "administrator", TargetID: id, Action: domain.ActionAdministratorInitialized,
			Result: domain.AuditSucceeded, SafeSummary: "administrator initialized locally"})
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Login 校验凭证并记录成功审计；仅供不需要单独建立会话的调用方使用。HTTP 登录应使用 Authenticate + RecordLogin，
// 以便只有在会话持久化成功后才记录 succeeded（FR-025/FR-027）。
func (s *AuthService) Login(ctx context.Context, username string, password []byte, source string) (*ports.AdministratorRecord, error) {
	admin, err := s.Authenticate(ctx, username, password, source)
	if err != nil {
		return nil, err
	}
	if err := s.RecordLogin(ctx, *admin); err != nil {
		return nil, err
	}
	return admin, nil
}

// Authenticate 校验用户名与密码并执行限流；失败与限流都写入 failed 审计，审计写入失败即返回错误（不得吞掉）。
// 不写 succeeded 审计：成功登录的审计由 RecordLogin 在会话确认建立后写入。
func (s *AuthService) Authenticate(ctx context.Context, username string, password []byte, source string) (*ports.AdministratorRecord, error) {
	normalized, normalizeErr := security.NormalizeUsername(username)
	if normalizeErr != nil {
		normalized = "invalid"
	}
	now := s.clock.Now()
	key := normalized + "|" + source
	if retry, limited := s.limited(key, now); limited {
		if err := s.RecordLoginFailure(ctx, "login throttled"); err != nil {
			return nil, err
		}
		return nil, &domain.RateLimitError{RetryAfter: retry}
	}
	var admin *ports.AdministratorRecord
	if err := s.store.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var err error
		admin, err = tx.Administrator(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	hash := s.dummyHash
	matchedUser := false
	if admin != nil && admin.Username == normalized && normalizeErr == nil {
		hash = admin.PasswordHash
		matchedUser = true
	}
	ok, err := security.ComparePassword(hash, password)
	if err != nil {
		return nil, err
	}
	if !matchedUser || !ok {
		if err := s.RecordLoginFailure(ctx, "login failed: invalid credentials"); err != nil {
			return nil, err
		}
		if retry, limited := s.recordFailure(key, now); limited {
			return nil, &domain.RateLimitError{RetryAfter: retry}
		}
		return nil, ErrInvalidCredentials
	}
	s.clearFailures(key, now)
	return admin, nil
}

// RecordLogin 在会话已持久化后写入 succeeded 登录审计。
func (s *AuthService) RecordLogin(ctx context.Context, admin ports.AdministratorRecord) error {
	return s.audit(ctx, admin, domain.ActionLogin, domain.AuditSucceeded, "administrator login succeeded")
}

// Logout 在会话确认撤销后写入 succeeded 登出审计；撤销失败时应改用 LogoutFailed。
func (s *AuthService) Logout(ctx context.Context, admin ports.AdministratorRecord) error {
	return s.audit(ctx, admin, domain.ActionLogout, domain.AuditSucceeded, "administrator logout succeeded")
}

// RevokeSessions 撤销管理员的全部会话：登录审计失败且当前会话撤销也失败时的最终补偿，确保不留下可用的部分认证状态。
func (s *AuthService) RevokeSessions(ctx context.Context, administratorID domain.ID) error {
	now := s.clock.Now()
	return s.store.WithWriteTx(ctx, func(tx ports.WriteTx) error {
		return tx.RevokeAllSessions(ctx, administratorID, now)
	})
}

// LogoutFailed 记录会话撤销失败的登出：审计结果为 failed，不得留下“登出成功但会话仍有效”的状态。
func (s *AuthService) LogoutFailed(ctx context.Context, admin ports.AdministratorRecord, summary string) error {
	return s.audit(ctx, admin, domain.ActionLogout, domain.AuditFailed, summary)
}

func (s *AuthService) SessionValid(ctx context.Context, administratorID domain.ID, passwordVersion int64) (bool, error) {
	var admin *ports.AdministratorRecord
	if err := s.store.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var err error
		admin, err = tx.Administrator(ctx)
		return err
	}); err != nil {
		return false, err
	}
	return admin != nil && admin.ID == administratorID && admin.PasswordVersion == passwordVersion, nil
}

func (s *AuthService) ResetPassword(ctx context.Context, password []byte) error {
	var current *ports.AdministratorRecord
	if err := s.store.WithReadTx(ctx, func(tx ports.ReadTx) error {
		var err error
		current, err = tx.Administrator(ctx)
		return err
	}); err != nil {
		return err
	}
	if current == nil {
		return &domain.NotFoundError{Resource: "administrator"}
	}
	if err := security.ValidatePassword(current.Username, password); err != nil {
		return &domain.ValidationError{Field: "password", Message: err.Error()}
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return err
	}
	now := s.clock.Now()
	return s.store.WithWriteTx(ctx, func(tx ports.WriteTx) error {
		if err := tx.UpdateAdministratorPassword(ctx, current.ID, hash, current.PasswordVersion+1, now); err != nil {
			return err
		}
		if err := tx.RevokeAllSessions(ctx, current.ID, now); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorLocalCLI,
			TargetType: "administrator", TargetID: current.ID, Action: domain.ActionPasswordReset,
			Result: domain.AuditSucceeded, SafeSummary: "administrator password reset locally"})
	})
}

func (s *AuthService) audit(ctx context.Context, admin ports.AdministratorRecord, action string, result domain.AuditResult, summary string) error {
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	return s.store.WithWriteTx(ctx, func(tx ports.WriteTx) error {
		actorID := admin.ID
		return tx.AppendAudit(ctx, domain.AuditEvent{ID: id, OccurredAt: s.clock.Now(), ActorType: domain.ActorAdministrator,
			ActorID: &actorID, TargetType: "administrator", TargetID: admin.ID, Action: action,
			Result: result, SafeSummary: summary})
	})
}

// RecordLoginFailure 记录失败、被限流或会话无法建立的登录：不区分用户名是否存在、不含密码，
// 目标固定为面板管理员身份（FR-025/FR-026）；写入失败即返回错误，由调用方拒绝请求。
func (s *AuthService) RecordLoginFailure(ctx context.Context, summary string) error {
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	return s.store.WithWriteTx(ctx, func(tx ports.WriteTx) error {
		target := domain.ID("00000000-0000-4000-8000-000000000000")
		if admin, err := tx.Administrator(ctx); err == nil && admin != nil {
			target = admin.ID
		}
		return tx.AppendAudit(ctx, domain.AuditEvent{ID: id, OccurredAt: s.clock.Now(), ActorType: domain.ActorSystem,
			TargetType: "administrator", TargetID: target, Action: domain.ActionLogin, Result: domain.AuditFailed, SafeSummary: summary})
	})
}

func (s *AuthService) prune(values []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-15 * time.Minute)
	first := 0
	for first < len(values) && values[first].Before(cutoff) {
		first++
	}
	return values[first:]
}

func (s *AuthService) limited(key string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bySource[key] = s.prune(s.bySource[key], now)
	s.global = s.prune(s.global, now)
	if len(s.bySource[key]) >= 5 {
		return boundedRetry(s.bySource[key][0].Add(15 * time.Minute).Sub(now)), true
	}
	if len(s.global) >= 20 {
		return boundedRetry(s.global[0].Add(15 * time.Minute).Sub(now)), true
	}
	return 0, false
}

func (s *AuthService) recordFailure(key string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bySource[key] = append(s.prune(s.bySource[key], now), now)
	s.global = append(s.prune(s.global, now), now)
	if len(s.bySource[key]) >= 5 || len(s.global) >= 20 {
		return 15 * time.Minute, true
	}
	return 0, false
}

func (s *AuthService) clearFailures(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bySource, key)
	s.global = s.prune(s.global, now)
}

func boundedRetry(value time.Duration) time.Duration {
	if value < time.Second {
		return time.Second
	}
	if value > 15*time.Minute {
		return 15 * time.Minute
	}
	return value
}
