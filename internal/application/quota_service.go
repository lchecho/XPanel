package application

import (
	"context"
	"log/slog"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/logging"
	"xpanel/internal/ports"
)

// 核心函数：QuotaService 负责周期切换（data-model §Atomic Transaction Boundaries 第 5 条与 §Write Ordering）。
//
// 职责：关闭到期周期、按当前策略与面板时区打开新周期；是否恢复访问由 Store 在事务内按最新事实决定，
//
//	只有“仅因配额超限”阻断的分配才会得到恢复操作；不负责采集与调额。
//
// 约束：切换幂等；停机跨越多个边界时按顺序补做，最终只保留一个 open 周期。
type QuotaService struct {
	store  ports.Store
	clock  ports.Clock
	notify func()
	logger *slog.Logger
}

func NewQuotaService(store ports.Store, clock ports.Clock, notify func(), logger *slog.Logger) *QuotaService {
	if logger == nil {
		logger = slog.Default()
	}
	return &QuotaService{store: store, clock: clock, notify: notify, logger: logger.With(logging.FieldComponent, "scheduler")}
}

// RolloverDue 结算所有结束时间不晚于 now 的 open 周期，返回跨越的边界总数；新周期边界由 Store 在事务内按最新策略与时区计算。
func (s *QuotaService) RolloverDue(ctx context.Context, now time.Time) (int, error) {
	due, err := s.store.DueCycles(ctx, now)
	if err != nil {
		return 0, err
	}
	rollovers := 0
	restoredAny := false
	for _, record := range due {
		ids := make([]domain.ID, 0, 2)
		for i := 0; i < 2; i++ {
			id, err := domain.NewID()
			if err != nil {
				return rollovers, err
			}
			ids = append(ids, id)
		}
		rollover := ports.CycleRollover{AllocationID: record.Allocation.ID, Now: now,
			OperationTemplate: domain.SynchronizationOperation{ID: ids[0], CreatedAt: now},
			Audit: &domain.AuditEvent{ID: ids[1], OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "user", TargetID: record.User.ID,
				Action: domain.ActionCycleRestored, Result: domain.AuditAccepted, SafeSummary: "new quota cycle opened; access restore requested"}}
		rolled, restored, err := s.store.RolloverCycle(ctx, rollover)
		if err != nil {
			return rollovers, err
		}
		if rolled > 0 {
			rollovers += rolled
			current, err := s.store.User(ctx, record.User.ID)
			if err != nil {
				return rollovers, err
			}
			s.logger.Info("quota cycle rolled over", logging.FieldAllocationID, record.Allocation.ID.String(), "boundaries", rolled,
				"cycle_start", current.Cycle.StartsAt.Format(time.RFC3339), "cycle_end", current.Cycle.EndsAt.Format(time.RFC3339),
				"restored", restored, logging.FieldResult, "succeeded")
		}
		if restored {
			restoredAny = true
		}
	}
	if restoredAny && s.notify != nil {
		s.notify()
	}
	return rollovers, nil
}

// NextBoundary 返回最近的周期边界，供 scheduler 设置定时器。
func (s *QuotaService) NextBoundary(ctx context.Context) (*time.Time, error) {
	return s.store.NextCycleEnd(ctx)
}
