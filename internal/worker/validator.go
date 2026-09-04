package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/logging"
	"xpanel/internal/ports"
)

// 核心函数：ProfileValidator 在事务外异步验证访问配置的兼容性。
//
// 职责：处理登记/编辑/手动重新验证排队的 profile，并周期性重试 unverified 与 unreachable 的 profile；
//
//	不负责变更 Xray 用户。
//
// 约束：验证与用户变更共用 node 锁，避免并发打到同一 Xray 实例。
type ProfileValidator struct {
	profiles *application.ProfileService
	store    ports.Store
	logger   *slog.Logger
	node     *sync.Mutex
	interval time.Duration
	queue    chan domain.ID
}

func NewProfileValidator(profiles *application.ProfileService, store ports.Store, logger *slog.Logger, node *sync.Mutex, interval time.Duration) *ProfileValidator {
	if node == nil {
		node = &sync.Mutex{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &ProfileValidator{profiles: profiles, store: store, logger: logger.With(logging.FieldComponent, "profile_validator"),
		node: node, interval: interval, queue: make(chan domain.ID, 64)}
}

// Enqueue 非阻塞地请求验证；队列满时由周期扫描兜底。
func (v *ProfileValidator) Enqueue(id domain.ID) {
	select {
	case v.queue <- id:
	default:
		v.logger.Warn("validation queue is full; relying on periodic sweep")
	}
}

func (v *ProfileValidator) Run(ctx context.Context) {
	ticker := time.NewTicker(v.interval)
	defer ticker.Stop()
	_ = v.SweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-v.queue:
			v.validate(ctx, id)
		case <-ticker.C:
			_ = v.SweepOnce(ctx)
		}
	}
}

// SweepOnce 重新验证所有 unverified 与 unreachable 的 profile。
func (v *ProfileValidator) SweepOnce(ctx context.Context) error {
	profiles, err := v.store.Profiles(ctx, false)
	if err != nil {
		return err
	}
	for _, record := range profiles {
		state := record.Profile.Compatibility
		if state == domain.CompatibilityUnverified || state == domain.CompatibilityUnreachable {
			v.validate(ctx, record.Profile.ID)
		}
	}
	return nil
}

// ValidateNow 同步验证一个 profile，供测试与启动流程使用。
func (v *ProfileValidator) ValidateNow(ctx context.Context, id domain.ID) error {
	v.node.Lock()
	defer v.node.Unlock()
	return v.profiles.RunValidation(ctx, id)
}

func (v *ProfileValidator) validate(ctx context.Context, id domain.ID) {
	started := time.Now()
	err := v.ValidateNow(ctx, id)
	if err != nil {
		v.logger.Warn("profile validation failed", "profile_id", id.String(), logging.FieldResult, "failed",
			logging.FieldErrorKind, "internal", logging.FieldDurationMS, time.Since(started).Milliseconds())
		return
	}
	v.logger.Info("profile validated", "profile_id", id.String(), logging.FieldResult, "completed",
		logging.FieldDurationMS, time.Since(started).Milliseconds())
}
