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

// 核心函数：ReconciliationService 比较 SQLite 期望状态与 Xray 实际入站/用户集合并修复漂移。
//
// 职责：列出 Xray 入站，为“应监听而未监听”“不应监听而在监听”的分配写入 reconcile 操作；把面板命名空间内
//
//	SQLite 无记录的孤立入站与未知客户端排队移除；不负责执行创建/移除（由 synchronizer 完成）。
//
// 约束：只在漂移时创建操作；有未完成操作的分配不重复入队；持续不同步超过 3×reconcile_interval 记 warn。
// AI-LOCK：非 `xpanel-` 前缀的入站与身份一律保留，不得修改或移除，也不计入面板统计（宪章 I/II）。
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
	// ConfirmedAbsent 是本轮为“移除永久失败但身份已不在 Xray（重启/外部移除）”的意图重新排队的确认次数。
	ConfirmedAbsent int
	StuckSync       int
	// Revalidated 是本轮因启动纪元变化而被置回待验证、需要重跑能力门禁的模板数。
	Revalidated int
	Reconnected bool
	Duration    time.Duration
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
	// 能力证据绑定当前启动纪元：节点重启（或纪元变化）后，旧的 compatible 结论对新进程不成立，
	// 必须先置回待验证并重跑门禁，再进入后续对账（FR-005/FR-029）。
	if stale, err := s.store.InvalidateStaleCapabilityEvidence(ctx, epochString(observation), now); err != nil {
		return summary, err
	} else if len(stale) > 0 {
		summary.Revalidated = len(stale)
		for _, id := range stale {
			s.logger.Warn("capability evidence is stale for the current Xray generation; template set back to unverified",
				"template_id", id.String(), logging.FieldResult, "queued")
			if s.revalidate != nil {
				_ = s.revalidate(ctx, id)
			}
		}
	}
	templates, err := s.store.Templates(ctx, false)
	if err != nil {
		return summary, err
	}
	users, err := s.store.ListUsers(ctx, ports.UserFilter{IncludeDeleted: true})
	if err != nil {
		return summary, err
	}
	inbounds, err := s.adapter.ListInbounds(ctx)
	if err != nil {
		s.markDegraded(ctx, err)
		return summary, err
	}
	remoteInbounds := make(map[string]bool, len(inbounds))
	for _, inbound := range inbounds {
		if inbound.PanelManaged {
			remoteInbounds[inbound.InboundTag] = true
		}
	}
	byTag := make(map[string]ports.UserRecord, len(users))
	for _, record := range users {
		byTag[record.Inbound.Inbound.InboundTag] = record
	}
	templateByID := make(map[domain.ID]ports.TemplateRecord, len(templates))
	enqueued := false
	for _, template := range templates {
		templateByID[template.Template.ID] = template
		summary.Profiles++
		if summary.Reconnected && template.Template.Compatibility == domain.CompatibilityUnreachable && s.revalidate != nil {
			_ = s.revalidate(ctx, template.Template.ID)
		}
	}

	// 面板命名空间内但 SQLite 无记录的入站：孤立入站，排队移除并在完成时释放端口（FR-031）。
	for tag := range remoteInbounds {
		if _, known := byTag[tag]; known {
			continue
		}
		// 孤立入站优先挂在一个模板下，以便沿用「未确认的移除冻结契约字段」这条既有规则；
		// 但没有可挂靠的模板时（一个模板都没有、全部归档或全部不兼容）也必须能排队清理，
		// 否则孤立入站会永远占着端口（FR-031）。
		created, err := s.store.EnqueueDriftRemoval(ctx, anyTemplateID(templates), tag, "inbound", tag, now)
		if err != nil {
			return summary, err
		}
		if created {
			summary.RemovedUnknown++
			enqueued = true
			s.logger.Info("orphan panel inbound queued for removal", logging.FieldResult, "queued")
		}
	}

	for _, record := range users {
		tag := record.Inbound.Inbound.InboundTag
		template, ok := templateByID[record.Allocation.TemplateID]
		// 只有「明确判定为不兼容」的模板才不投影。unverified 也要投影：节点重启后能力证据会被
		// 置为待验证，但既有用户的入站必须照常按原端口重建（FR-030/SC-006）——
		// 能力门禁把关的是「能不能创建新用户」，不是「要不要恢复已有用户」。
		if !ok || template.Template.Compatibility == domain.CompatibilityIncompatible {
			continue
		}
		actual := remoteInbounds[tag]
		if actual {
			// 入站存在时进一步确认其中的受管客户端；多余的面板客户端按未知身份排队移除。
			remote, err := s.adapter.ListUsers(ctx, ports.RuntimeInbound{TemplateID: template.Template.ID,
				InboundTag: tag, Method: template.Template.Method})
			if err != nil {
				s.markDegraded(ctx, err)
				return summary, err
			}
			// 专属入站里只应有该分配的期望身份。除它之外的任何客户端都是漂移——
			// 包括没有 xpanel- 前缀的外部身份：它们同样能用未知密钥经这个端口出网（宪章 I/II、FR-019）。
			// 唯一豁免是轮换过渡身份，且仅当该分配确实有一条未完成的轮换意图时；意图收敛后它必须被清理。
			rotating := false
			if err := func() error {
				open, err := s.store.HasOpenOperation(ctx, record.Allocation.ID)
				rotating = open
				return err
			}(); err != nil {
				return summary, err
			}
			expected := false
			for _, user := range remote {
				if !user.Present {
					continue
				}
				if user.StatisticsID == record.Identity.StatisticsID {
					expected = true
					continue
				}
				if rotating && user.StatisticsID == domain.RotationSafetyID(record.Identity.StatisticsID) {
					continue // 轮换进行中的有界过渡身份，由同步器在收敛时清理
				}
				created, err := s.store.EnqueueDriftRemoval(ctx, template.Template.ID, tag, "identity", user.StatisticsID, now)
				if err != nil {
					return summary, err
				}
				if created {
					summary.RemovedUnknown++
					enqueued = true
					s.logger.Info("unknown identity in a panel inbound queued for removal", logging.FieldNodeID, template.Template.InstanceID.String(),
						"template_id", template.Template.ID.String(), logging.FieldInboundTag, tag, logging.FieldResult, "queued")
				}
			}
			actual = expected
		}
		desired := record.Allocation.DesiredPresent(record.User)
		if err := s.store.RecordObservation(ctx, record.Allocation.ID, actual, now); err != nil {
			return summary, err
		}
		if err := s.store.ConfirmInboundPresence(ctx, record.Allocation.ID, actual, now); err != nil {
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
			domain.SyncReconcile, domain.InboundPhaseFor(desired), now)
		queued, err := s.store.EnqueueReconcile(ctx, record.Allocation.ID, record.Allocation.DesiredRevision, op, now)
		if err != nil {
			return summary, err
		}
		if queued {
			summary.Drift++
			enqueued = true
			s.logger.Info("drift detected; reconcile operation queued", logging.FieldAllocationID, record.Allocation.ID.String(),
				logging.FieldOperationID, opID.String(), logging.FieldTargetState, targetState(desired), logging.FieldResult, "queued")
		}
	}

	// 永久失败的移除意图仍冻结模板契约字段；目标已因重启或外部移除而不在 Xray 时，
	// 通过可租约的确认路径（重新排队 → synchronizer 读后确认 absent 并审计）收敛，避免模板永久锁死（FR-020）。
	for _, template := range templates {
		stale, err := s.store.StaleDriftRemovals(ctx, template.Template.ID)
		if err != nil {
			return summary, err
		}
		for _, removal := range stale {
			if remoteInbounds[removal.InboundTag] {
				continue // 目标仍在：上面的分支已重新排队移除
			}
			created, err := s.store.EnqueueDriftRemoval(ctx, template.Template.ID, removal.InboundTag, removal.Kind, removal.StatisticsID, now)
			if err != nil {
				return summary, err
			}
			if created {
				summary.ConfirmedAbsent++
				enqueued = true
				s.logger.Info("permanently failed drift removal re-queued for absence confirmation",
					"template_id", template.Template.ID.String(), logging.FieldResult, "queued")
			}
		}
	}
	// 没有归属模板的永久失败孤立入站同样要重新排队确认，否则它们会永远留在 Xray 里占着端口。
	orphans, err := s.store.OrphanStaleDriftRemovals(ctx)
	if err != nil {
		return summary, err
	}
	for _, removal := range orphans {
		if remoteInbounds[removal.InboundTag] {
			continue // 目标仍在：上面的分支已重新排队移除
		}
		created, err := s.store.EnqueueDriftRemoval(ctx, "", removal.InboundTag, removal.Kind, removal.StatisticsID, now)
		if err != nil {
			return summary, err
		}
		if created {
			summary.ConfirmedAbsent++
			enqueued = true
			s.logger.Info("permanently failed orphan inbound removal re-queued for absence confirmation",
				logging.FieldInboundTag, removal.InboundTag, logging.FieldResult, "queued")
		}
	}
	if enqueued && s.notify != nil {
		s.notify()
	}
	summary.Duration = s.clock.Now().Sub(started)
	s.logger.Info("reconciliation finished", "templates", summary.Profiles, "drift", summary.Drift, "removed_unknown", summary.RemovedUnknown, "confirmed_absent", summary.ConfirmedAbsent,
		"stuck", summary.StuckSync, logging.FieldResult, "succeeded", logging.FieldDurationMS, summary.Duration.Milliseconds())
	return summary, nil
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

// anyTemplateID 为孤立入站的移除意图选择一个归属模板：孤立入站不属于任何用户，
// 意图只需要一个稳定的归属点以复用既有的租约与因果链机制；没有可用模板时返回空，
// 此时意图以「无归属」持久化，同样可以被领取和执行。
func anyTemplateID(templates []ports.TemplateRecord) domain.ID {
	for _, template := range templates {
		if template.Template.ArchivedAt != nil {
			continue
		}
		if template.Template.Compatibility == domain.CompatibilityCompatible ||
			template.Template.Compatibility == domain.CompatibilityUnreachable {
			return template.Template.ID
		}
	}
	return ""
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
