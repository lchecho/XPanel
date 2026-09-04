package worker

import (
	"context"
	"log/slog"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/logging"
	"xpanel/internal/ports"
)

// 核心函数：Scheduler 在周期边界时刻触发配额周期切换。
// 职责：边界定时器 + 每 60 秒兜底扫描（data-model §Write Ordering）；启动时先补做漏执行的边界。
// 约束：切换本身幂等，由 QuotaService.RolloverDue 保证；调度器只负责“何时”。
type Scheduler struct {
	quota  *application.QuotaService
	clock  ports.Clock
	sweep  time.Duration
	logger *slog.Logger
}

func NewScheduler(quota *application.QuotaService, clock ports.Clock, sweep time.Duration, logger *slog.Logger) *Scheduler {
	if sweep <= 0 {
		sweep = time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{quota: quota, clock: clock, sweep: sweep, logger: logger.With(logging.FieldComponent, "scheduler")}
}

func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.sweep)
	defer ticker.Stop()
	for {
		s.RunOnce(ctx)
		wait := s.sweep
		if next, err := s.quota.NextBoundary(ctx); err == nil && next != nil {
			if until := next.Sub(s.clock.Now()); until > 0 && until < wait {
				wait = until
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-ticker.C:
			timer.Stop()
		}
	}
}

// RunOnce 结算所有到期周期。
func (s *Scheduler) RunOnce(ctx context.Context) {
	started := time.Now()
	count, err := s.quota.RolloverDue(ctx, s.clock.Now())
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("cycle rollover failed", logging.FieldResult, "failed", logging.FieldErrorKind, "internal")
		}
		return
	}
	if count > 0 {
		s.logger.Info("cycle rollover finished", "rollovers", count, logging.FieldResult, "succeeded",
			logging.FieldDurationMS, time.Since(started).Milliseconds())
	}
}
