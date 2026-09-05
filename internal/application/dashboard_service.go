package application

import (
	"context"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// 核心函数：DashboardService 汇总仪表盘与详情页所需的只读数据；只读 SQLite，绝不调用 Xray。
type DashboardService struct {
	store    ports.Store
	clock    ports.Clock
	interval time.Duration
}

func NewDashboardService(store ports.Store, clock ports.Clock, interval time.Duration) *DashboardService {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &DashboardService{store: store, clock: clock, interval: interval}
}

// DashboardSummary 是仪表盘汇总；Stale 表示最后采集成功早于两个采集周期或最近探测失败。
type DashboardSummary struct {
	Instance         ports.ManagedInstanceRecord
	LastCollectionAt *time.Time
	Stale            bool
	TotalUsers       int
	Active           int
	Disabled         int
	QuotaExceeded    int
	Pending          int
	Deleted          int
	AccountedBytes   int64
	Failed           []ports.FailedOperationRecord
	GeneratedAt      time.Time
}

func (s *DashboardService) Summary(ctx context.Context) (DashboardSummary, error) {
	now := s.clock.Now()
	summary := DashboardSummary{GeneratedAt: now}
	instance, err := s.store.ManagedInstance(ctx)
	if err != nil {
		return summary, err
	}
	summary.Instance = instance
	last, err := s.store.LastCollectionAt(ctx)
	if err != nil {
		return summary, err
	}
	summary.LastCollectionAt = last
	summary.Stale = s.isStale(instance, last, now)
	users, err := s.store.ListUsers(ctx, ports.UserFilter{IncludeDeleted: true})
	if err != nil {
		return summary, err
	}
	for _, record := range users {
		state := record.Allocation.DisplayState(record.User)
		switch state {
		case domain.DisplayDeleted:
			summary.Deleted++
			continue
		case domain.DisplayActive:
			summary.Active++
		case domain.DisplayDisabled, domain.DisplayDisabling:
			summary.Disabled++
		case domain.DisplayQuotaExceeded, domain.DisplayQuotaDisabling:
			summary.QuotaExceeded++
		}
		summary.TotalUsers++
		if record.Allocation.PendingSync() {
			summary.Pending++
		}
		summary.AccountedBytes += record.Cycle.AccountedUplinkBytes + record.Cycle.AccountedDownlinkBytes
	}
	summary.Failed, err = s.store.FailedOperations(ctx, 5)
	return summary, err
}

// isStale 实现 http.md 的陈旧判定：最后采集成功早于 2×traffic_interval，或实例最近探测失败。
func (s *DashboardService) isStale(instance ports.ManagedInstanceRecord, last *time.Time, now time.Time) bool {
	if instance.HealthState == "unreachable" || instance.HealthState == "incompatible" {
		return true
	}
	if last == nil {
		return instance.LastSuccessAt == nil || now.Sub(*instance.LastSuccessAt) > 2*s.interval
	}
	return now.Sub(*last) > 2*s.interval
}

// UserDetail 是详情页的扩展数据：当前周期日趋势、连续性事件与同步操作历史。
type UserDetail struct {
	Record ports.UserRecord
	Daily  []ports.DailyAggregateRecord
	Events []ports.ContinuityEventListRecord
	Sync   []domain.SynchronizationOperation
}

func (s *DashboardService) UserDetail(ctx context.Context, record ports.UserRecord) (UserDetail, error) {
	detail := UserDetail{Record: record}
	var err error
	if detail.Daily, err = s.store.DailyAggregates(ctx, record.Allocation.ID, record.Cycle.StartsAt, record.Cycle.EndsAt); err != nil {
		return detail, err
	}
	if detail.Events, err = s.store.ContinuityEvents(ctx, record.Allocation.ID, 20); err != nil {
		return detail, err
	}
	detail.Sync, err = s.store.SyncOperations(ctx, record.Allocation.ID, 10)
	return detail, err
}
