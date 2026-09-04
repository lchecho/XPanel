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
// 职责：关闭到期周期、按当前策略与面板时区打开新周期、清除配额阻断并为“仅因超限”阻断的分配写恢复操作；
//
//	不负责采集与调额（分别在 TrafficService 与 UserService）。
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
	restored := false
	for _, record := range due {
		cycle := record.Cycle
		wasExceeded := record.Allocation.QuotaState == domain.QuotaExceeded
		revision := record.Allocation.DesiredRevision
		for !cycle.EndsAt.After(now) {
			start := cycle.EndsAt
			_, end, err := domain.CycleBounds(start, location, record.Policy.ResetDay)
			if err != nil {
				return rollovers, err
			}
			newID, err := domain.NewID()
			if err != nil {
				return rollovers, err
			}
			next := ports.QuotaCycleRecord{ID: newID, AllocationID: record.Allocation.ID, StartsAt: start, EndsAt: end,
				Timezone: settings.QuotaTimezone, ResetDay: record.Policy.ResetDay, Status: "open", OpenedAt: now}
			rollover := ports.CycleRollover{AllocationID: record.Allocation.ID, OldCycleID: cycle.ID, NewCycle: next, Now: now}
			if wasExceeded {
				// 只有“仅因配额超限”而不可用的分配才自动恢复（spec FR-018）。
				if record.Allocation.AdminEnabled && record.User.Lifecycle == domain.LifecycleActive {
					opID, err := domain.NewID()
					if err != nil {
						return rollovers, err
					}
					revision++
					version := record.Allocation.DesiredCredentialVersion
					op := domain.NewSynchronizationOperation(opID, record.Allocation.ID, revision, true, &version, domain.SyncQuotaRestore, domain.SyncAddDesired, now)
					rollover.Operation = &op
					auditID, err := domain.NewID()
					if err != nil {
						return rollovers, err
					}
					rollover.Audit = &domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "user",
						TargetID: record.User.ID, Action: domain.ActionCycleRestored, Result: domain.AuditAccepted, OperationID: &opID,
						SafeSummary: "new quota cycle opened; access restore requested"}
					restored = true
				}
				wasExceeded = false
			}
			if err := s.store.RolloverCycle(ctx, rollover); err != nil {
				return rollovers, err
			}
			rollovers++
			s.logger.Info("quota cycle rolled over", logging.FieldAllocationID, record.Allocation.ID.String(),
				"cycle_start", start.Format(time.RFC3339), "cycle_end", end.Format(time.RFC3339), logging.FieldResult, "succeeded")
			cycle = next
		}
	}
	if restored && s.notify != nil {
		s.notify()
	}
	return rollovers, nil
}

// NextBoundary 返回最近的周期边界，供 scheduler 设置定时器。
func (s *QuotaService) NextBoundary(ctx context.Context) (*time.Time, error) {
	return s.store.NextCycleEnd(ctx)
}
