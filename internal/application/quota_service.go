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

// RolloverDue 结算所有结束时间不晚于 now 的 open 周期，返回切换次数。
func (s *QuotaService) RolloverDue(ctx context.Context, now time.Time) (int, error) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return 0, err
	}
	location, err := time.LoadLocation(settings.QuotaTimezone)
	if err != nil {
		location = time.UTC
	}
	due, err := s.store.DueCycles(ctx, now)
	if err != nil {
		return 0, err
	}
	rollovers := 0
	restoredAny := false
	for _, record := range due {
		cycle := record.Cycle
		for !cycle.EndsAt.After(now) {
			start := cycle.EndsAt
			_, end, err := domain.CycleBounds(start, location, record.Policy.ResetDay)
			if err != nil {
				return rollovers, err
			}
			ids := make([]domain.ID, 0, 3)
			for i := 0; i < 3; i++ {
				id, err := domain.NewID()
				if err != nil {
					return rollovers, err
				}
				ids = append(ids, id)
			}
			next := ports.QuotaCycleRecord{ID: ids[0], AllocationID: record.Allocation.ID, StartsAt: start, EndsAt: end,
				Timezone: settings.QuotaTimezone, ResetDay: record.Policy.ResetDay, Status: "open", OpenedAt: now}
			rollover := ports.CycleRollover{AllocationID: record.Allocation.ID, OldCycleID: cycle.ID, NewCycle: next, Now: now,
				OperationTemplate: domain.SynchronizationOperation{ID: ids[1], CreatedAt: now},
				Audit: &domain.AuditEvent{ID: ids[2], OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "user", TargetID: record.User.ID,
					Action: domain.ActionCycleRestored, Result: domain.AuditAccepted, SafeSummary: "new quota cycle opened; access restore requested"}}
			rolled, restored, err := s.store.RolloverCycle(ctx, rollover)
			if err != nil {
				return rollovers, err
			}
			if rolled {
				rollovers++
				s.logger.Info("quota cycle rolled over", logging.FieldAllocationID, record.Allocation.ID.String(),
					"cycle_start", start.Format(time.RFC3339), "cycle_end", end.Format(time.RFC3339), "restored", restored, logging.FieldResult, "succeeded")
			}
			if restored {
				restoredAny = true
			}
			cycle = next
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
