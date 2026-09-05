package application

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/logging"
	"xpanel/internal/ports"
)

// 核心函数：ReconciliationService 比较 SQLite 期望状态与 Xray 实际用户集合并修复漂移。
//
// 职责：按 profile 列出 Xray 用户，为“应在而不在”“不应在而在”的分配写入 reconcile 操作，直接移除面板命名空间内
//
//	SQLite 无记录的身份，保留 bootstrap 与非面板身份；不负责执行 add/remove（由 synchronizer 完成）。
//
// 约束：只在漂移时创建操作；有未完成操作的分配不重复入队；持续不同步超过 3×reconcile_interval 记 warn。
// AI-LOCK：非 `xpanel-` 前缀的身份（含 bootstrap）一律保留，不计入面板统计。
type ReconciliationService struct {
	store      ports.Store
	adapter    ports.Adapter
	clock      ports.Clock
	target     ports.InstanceTarget
	node       *sync.Mutex
	interval   time.Duration
	notify     func()
	revalidate func(context.Context, domain.ID) error
	logger     *slog.Logger

	mu       sync.Mutex
	degraded bool
}

type ReconcileSummary struct {
	Profiles       int
	Drift          int
	RemovedUnknown int
	StuckSync      int
	Reconnected    bool
	Duration       time.Duration
}

func NewReconciliationService(store ports.Store, adapter ports.Adapter, clock ports.Clock, target ports.InstanceTarget, node *sync.Mutex,
	interval time.Duration, notify func(), revalidate func(context.Context, domain.ID) error, logger *slog.Logger) *ReconciliationService {
	if node == nil {
		node = &sync.Mutex{}
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ReconciliationService{store: store, adapter: adapter, clock: clock, target: target, node: node, interval: interval,
		notify: notify, revalidate: revalidate, logger: logger.With(logging.FieldComponent, "reconciler")}
}

// ReconcileOnce 执行一轮协调。Xray 不可达时只记录实例健康并返回错误。
func (s *ReconciliationService) ReconcileOnce(ctx context.Context) (ReconcileSummary, error) {
	started := s.clock.Now()
	summary := ReconcileSummary{}
	s.node.Lock()
	defer s.node.Unlock()
	observation, err := s.adapter.Probe(ctx, s.target)
	if err != nil {
		s.markDegraded(ctx, err)
		return summary, err
	}
	summary.Reconnected = s.clearDegraded()
	now := s.clock.Now()
	if err := s.store.MarkInstanceHealthy(ctx, epochString(observation), now); err != nil {
		return summary, err
	}
	profiles, err := s.store.Profiles(ctx, false)
	if err != nil {
		return summary, err
	}
	users, err := s.store.ListUsers(ctx, ports.UserFilter{IncludeDeleted: true})
	if err != nil {
		return summary, err
	}
	byIdentity := make(map[string]ports.UserRecord, len(users))
	for _, record := range users {
		byIdentity[record.Identity.StatisticsID] = record
	}
	enqueued := false
	for _, profile := range profiles {
		if profile.Profile.Compatibility != domain.CompatibilityCompatible && profile.Profile.Compatibility != domain.CompatibilityUnreachable {
			continue
		}
		if summary.Reconnected && profile.Profile.Compatibility == domain.CompatibilityUnreachable && s.revalidate != nil {
			_ = s.revalidate(ctx, profile.Profile.ID)
		}
		runtimeProfile := ports.RuntimeProfile{ID: profile.Profile.ID, InboundTag: profile.Profile.InboundTag, Method: profile.Profile.Method,
			BootstrapStatisticsID: profile.Profile.BootstrapStatisticsID}
		remote, err := s.adapter.ListUsers(ctx, runtimeProfile)
		if err != nil {
			s.markDegraded(ctx, err)
			return summary, err
		}
		summary.Profiles++
		present := make(map[string]bool, len(remote))
		for _, user := range remote {
			if !user.Present {
				continue
			}
			present[user.StatisticsID] = true
			if user.Kind != "managed" || user.StatisticsID == profile.Profile.BootstrapStatisticsID {
				continue
			}
			if _, known := byIdentity[user.StatisticsID]; known {
				continue
			}
			// 面板命名空间内但 SQLite 无记录：先持久化移除意图，由 synchronizer 在事务外串行执行并审计（Constitution I/IV）。
			created, err := s.store.EnqueueDriftRemoval(ctx, profile.Profile.ID, user.StatisticsID, now)
			if err != nil {
				return summary, err
			}
			if created {
				summary.RemovedUnknown++
				enqueued = true
				s.logger.Info("unknown namespace identity queued for removal", logging.FieldNodeID, profile.Profile.InstanceID.String(),
					"profile_id", profile.Profile.ID.String(), logging.FieldResult, "queued")
			}
		}
		for _, record := range users {
			if record.Allocation.ProfileID != profile.Profile.ID {
				continue
			}
			actual := present[record.Identity.StatisticsID]
			desired := record.Allocation.DesiredPresent(record.User)
			if err := s.store.RecordObservation(ctx, record.Allocation.ID, actual, now); err != nil {
				return summary, err
			}
			if record.Allocation.PendingSync() {
				if now.Sub(record.Allocation.UpdatedAt) > 3*s.interval {
					summary.StuckSync++
					s.logger.Warn("allocation remains out of sync", logging.FieldAllocationID, record.Allocation.ID.String(),
						logging.FieldTargetState, targetState(desired), logging.FieldResult, "stuck")
				}
				continue
			}
			if actual == desired {
				continue
			}
			open, err := s.store.HasOpenOperation(ctx, record.Allocation.ID)
			if err != nil {
				return summary, err
			}
			if open {
				continue
			}
			opID, err := domain.NewID()
			if err != nil {
				return summary, err
			}
			version := record.Allocation.DesiredCredentialVersion
			op := domain.NewSynchronizationOperation(opID, record.Allocation.ID, record.Allocation.DesiredRevision+1, desired, &version,
				domain.SyncReconcile, phaseFor(desired), now)
			ok, err := s.store.EnqueueReconcile(ctx, record.Allocation.ID, record.Allocation.DesiredRevision, op, now)
			if err != nil {
				return summary, err
			}
			if ok {
				summary.Drift++
				enqueued = true
				s.logger.Info("drift detected; reconcile operation queued", logging.FieldAllocationID, record.Allocation.ID.String(),
					logging.FieldOperationID, opID.String(), logging.FieldTargetState, targetState(desired), logging.FieldResult, "queued")
			}
		}
	}
	if enqueued && s.notify != nil {
		s.notify()
	}
	summary.Duration = s.clock.Now().Sub(started)
	s.logger.Info("reconciliation finished", "profiles", summary.Profiles, "drift", summary.Drift, "removed_unknown", summary.RemovedUnknown,
		"stuck", summary.StuckSync, logging.FieldResult, "succeeded", logging.FieldDurationMS, summary.Duration.Milliseconds())
	return summary, nil
}

func phaseFor(present bool) domain.SyncPhase {
	if present {
		return domain.SyncAddDesired
	}
	return domain.SyncRemoveOld
}

func targetState(present bool) string {
	if present {
		return "present"
	}
	return "absent"
}

func adapterKind(err error) (string, string) {
	var adapterErr *ports.AdapterError
	if errors.As(err, &adapterErr) {
		return adapterErr.Kind, adapterErr.SafeSummary
	}
	return ports.ErrorInternal, "Xray operation failed"
}

func (s *ReconciliationService) markDegraded(ctx context.Context, cause error) {
	kind, summary := adapterKind(cause)
	s.mu.Lock()
	s.degraded = true
	s.mu.Unlock()
	if err := s.store.MarkInstanceUnreachable(ctx, kind, summary, s.clock.Now()); err != nil {
		s.logger.Warn("record instance health", logging.FieldErrorKind, "internal")
	}
	s.logger.Warn("reconciliation skipped: Xray unavailable", logging.FieldResult, "failed", logging.FieldErrorKind, kind)
}

func (s *ReconciliationService) clearDegraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.degraded
	s.degraded = false
	return was
}
