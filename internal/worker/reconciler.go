package worker

import (
	"context"
	"log/slog"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/logging"
)

// 核心函数：Reconciler 在启动、Xray 重连与固定周期运行协调。
// 职责：调度 ReconciliationService.ReconcileOnce；与 synchronizer 共用 node 锁由服务层保证。
// 约束：reconcile_interval ≤ 30 秒以满足 60 秒收敛目标；Trigger 允许其他组件（如同步恢复）立即触发一轮。
type Reconciler struct {
	service  *application.ReconciliationService
	interval time.Duration
	logger   *slog.Logger
	trigger  chan struct{}
}

func NewReconciler(service *application.ReconciliationService, interval time.Duration, logger *slog.Logger) *Reconciler {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{service: service, interval: interval, logger: logger.With(logging.FieldComponent, "reconciler"), trigger: make(chan struct{}, 1)}
}

// Trigger 非阻塞地请求立即协调（例如 synchronizer 观察到 Xray 恢复时）。
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	r.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
			r.RunOnce(ctx)
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

func (r *Reconciler) RunOnce(ctx context.Context) {
	if _, err := r.service.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
		r.logger.Debug("reconciliation round failed", logging.FieldResult, "failed")
	}
}
