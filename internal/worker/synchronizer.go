package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/logging"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 核心函数：Synchronizer 把 SQLite 中的期望状态投影到 Xray。
//
// 职责：串行领取到期的同步操作，在事务外调用 Adapter，再用 revision 条件确认；
//
//	不负责决定期望状态（由 application 层写入操作）。
//
// 约束：任何 Xray 变更只在持有 node 锁时执行；每次 Xray 调用前必须续租并重新确认租约与最新意图（fence），
//
//	丢失租约即放弃（不再调用、不再确认）；不确定结果先读后写；退避有界（max_retry_interval）。
//
// AI-LOCK：不得在数据库事务内调用 Adapter；不得在 revision 已变化或租约不属于本 worker 时确认旧操作。
//
// 下游：Xray via gRPC（经 ports.Adapter）
// 失败处理：可重试错误按 full jitter 指数退避；不可重试错误标记 permanent_failed 并审计。
type Synchronizer struct {
	store    ports.Store
	adapter  ports.Adapter
	keyring  *security.Keyring
	clock    ports.Clock
	logger   *slog.Logger
	node     *sync.Mutex
	owner    string
	maxRetry time.Duration
	lease    time.Duration
	poll     time.Duration
	random   func(int64) int64
	wake     chan struct{}

	onProfileRecovered func(domain.ID)

	mu       sync.Mutex
	failures int
}

type SynchronizerOptions struct {
	Owner            string
	MaxRetryInterval time.Duration
	LeaseDuration    time.Duration
	// RPCTimeout 是单次 Xray 调用的上限；租约至少覆盖 3 次 RPC，保证调用期间租约不会自然到期。
	RPCTimeout         time.Duration
	PollInterval       time.Duration
	Random             func(int64) int64
	OnProfileRecovered func(domain.ID)
}

const (
	maxInternalAttempts   = 20
	unreachableThreshold  = 3
	defaultLeaseDuration  = 30 * time.Second
	defaultPollInterval   = time.Second
	defaultMaxRetryPeriod = 30 * time.Second
	// defaultRPCTimeout 与 config 默认 rpc_timeout 一致；所有构造路径都必须有明确的 RPC 超时以推导租约下限。
	defaultRPCTimeout = 5 * time.Second
	// leaseRPCMultiple 保证租约覆盖单次 RPC 及其读后写：lease ≥ 3 × RPC 超时。
	leaseRPCMultiple = 3
)

func NewSynchronizer(store ports.Store, adapter ports.Adapter, keyring *security.Keyring, clock ports.Clock,
	logger *slog.Logger, node *sync.Mutex, options SynchronizerOptions) *Synchronizer {
	if options.Owner == "" {
		options.Owner = "synchronizer"
	}
	if options.MaxRetryInterval <= 0 {
		options.MaxRetryInterval = defaultMaxRetryPeriod
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaultLeaseDuration
	}
	if options.RPCTimeout <= 0 {
		options.RPCTimeout = defaultRPCTimeout
	}
	if options.LeaseDuration < leaseRPCMultiple*options.RPCTimeout {
		options.LeaseDuration = leaseRPCMultiple * options.RPCTimeout
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaultPollInterval
	}
	if node == nil {
		node = &sync.Mutex{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Synchronizer{store: store, adapter: adapter, keyring: keyring, clock: clock,
		logger: logger.With(logging.FieldComponent, "synchronizer"), node: node, owner: options.Owner,
		maxRetry: options.MaxRetryInterval, lease: options.LeaseDuration, poll: options.PollInterval,
		random: options.Random, wake: make(chan struct{}, 1), onProfileRecovered: options.OnProfileRecovered}
}

// Wake 由提交了同步操作的事务在提交后调用，立即唤醒 worker（非阻塞）。
func (s *Synchronizer) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run 循环直到 ctx 取消：被唤醒或按兜底轮询间隔领取并处理全部到期操作。
func (s *Synchronizer) Run(ctx context.Context) {
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if _, err := s.Drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn("synchronizer drain failed", logging.FieldErrorKind, "internal")
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

// Drain 处理当前所有到期操作，返回处理数量。测试与启动时直接调用。
func (s *Synchronizer) Drain(ctx context.Context) (int, error) {
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		work, err := s.store.LeaseDueSync(ctx, s.owner, s.clock.Now(), s.lease)
		if err != nil {
			return processed, err
		}
		if work == nil {
			break
		}
		processed++
		if err := s.handle(ctx, work); err != nil {
			return processed, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		removal, err := s.store.LeaseDueDriftRemoval(ctx, s.owner, s.clock.Now(), s.lease)
		if err != nil {
			return processed, err
		}
		if removal == nil {
			return processed, nil
		}
		processed++
		if err := s.handleDriftRemoval(ctx, removal); err != nil {
			return processed, err
		}
	}
}

// handleDriftRemoval 执行一条持久化的漂移移除意图：删除不存在视为收敛；不确定结果读后写；可重试错误退避。
func (s *Synchronizer) handleDriftRemoval(ctx context.Context, removal *ports.DriftRemoval) error {
	s.node.Lock()
	defer s.node.Unlock()
	now := s.clock.Now()
	logger := s.logger.With("drift_removal_id", removal.ID.String(), "profile_id", removal.ProfileID.String(), logging.FieldTargetState, "absent")
	profile := ports.RuntimeProfile{ID: removal.ProfileID, InboundTag: removal.InboundTag}
	if held, err := s.fenceDrift(ctx, removal, logger); err != nil || !held {
		return err
	}
	if removal.Reclaimed {
		// 回收过期租约：前一持有者的 RemoveUser 可能已经生效（RPC 成功后崩溃）。先读原入站的实际身份，
		// 已不存在则直接完成，不再重复调用外部移除（FR-021）。
		present, observeErr := s.observe(ctx, profile, removal.StatisticsID)
		if observeErr == nil && !present {
			logger.Info("unknown namespace identity already absent after lease reclaim", logging.FieldResult, "succeeded")
			return s.store.CompleteDriftRemoval(ctx, removal.ID, s.owner, now,
				s.driftAudit(removal, domain.AuditSucceeded, "unknown identity "+removal.StatisticsID+" confirmed absent after lease reclaim", now))
		}
	}
	_, err := s.adapter.RemoveUser(ctx, ports.RemoveUserCommand{OperationID: removal.ID, ProfileTag: removal.InboundTag, StatisticsID: removal.StatisticsID})
	summary := "removed unknown identity " + removal.StatisticsID + " from the managed namespace"
	if err != nil {
		kind, retryable := describe(err)
		converged := kind == ports.ErrorUserNotFound
		if converged {
			summary = "unknown identity " + removal.StatisticsID + " confirmed absent from the managed namespace"
		}
		if !converged && retryable && kind != ports.ErrorInstanceUnavailable {
			if held, err := s.fenceDrift(ctx, removal, logger); err != nil || !held {
				return err
			}
			present, observeErr := s.observe(ctx, profile, removal.StatisticsID)
			converged = observeErr == nil && !present
		}
		if !converged {
			summary := summaryOf(err)
			attempts := removal.AttemptCount + 1
			if !retryable || (kind == ports.ErrorInternal && attempts >= maxInternalAttempts) {
				logger.Error("drift removal failed permanently", logging.FieldResult, "permanent_failed", logging.FieldErrorKind, kind)
				return s.store.FailDriftRemoval(ctx, removal.ID, s.owner, kind, summary, now, s.driftAudit(removal, domain.AuditFailed, "removal of unknown identity failed: "+summary, now))
			}
			next := now.Add(domain.NextBackoff(attempts, s.maxRetry, s.random))
			logger.Warn("drift removal retry scheduled", logging.FieldResult, "retry_wait", logging.FieldErrorKind, kind, "attempt", attempts)
			return s.store.RescheduleDriftRemoval(ctx, removal.ID, s.owner, attempts, next, kind, summary)
		}
	}
	logger.Info("unknown namespace identity removed", logging.FieldResult, "succeeded")
	return s.store.CompleteDriftRemoval(ctx, removal.ID, s.owner, now, s.driftAudit(removal, domain.AuditSucceeded, summary, now))
}

// fenceDrift 在每次 Xray 调用前续租；租约丢失时记录并放弃（返回 false）。
func (s *Synchronizer) fenceDrift(ctx context.Context, removal *ports.DriftRemoval, logger *slog.Logger) (bool, error) {
	held, err := s.store.RenewDriftRemovalLease(ctx, removal.ID, s.owner, s.clock.Now(), s.lease)
	if err != nil {
		return false, err
	}
	if !held {
		logger.Warn("drift removal lease lost; abandoning without calling Xray", logging.FieldResult, "abandoned")
	}
	return held, nil
}

// fence 在每次 Xray 调用前续租并重新确认最新意图；租约丢失或意图已变化时放弃本操作（不再调用、不再确认 Xray）。
func (s *Synchronizer) fence(ctx context.Context, work *ports.SyncWork, logger *slog.Logger) (bool, error) {
	held, err := s.store.RenewSyncLease(ctx, work.Operation.ID, s.owner, s.clock.Now(), s.lease)
	if err != nil {
		return false, err
	}
	if !held {
		logger.Warn("synchronization lease lost or intent changed; abandoning without calling Xray", logging.FieldResult, "abandoned")
	}
	return held, nil
}

func (s *Synchronizer) driftAudit(removal *ports.DriftRemoval, result domain.AuditResult, summary string, now time.Time) domain.AuditEvent {
	id, _ := domain.NewID()
	operationID := removal.ID
	return domain.AuditEvent{ID: id, OccurredAt: now, ActorType: domain.ActorSystem, TargetType: "profile", TargetID: removal.ProfileID,
		Action: domain.ActionReconcileRemovedUnknown, Result: result, OperationID: &operationID, SafeSummary: summary}
}

func (s *Synchronizer) handle(ctx context.Context, work *ports.SyncWork) error {
	s.node.Lock()
	defer s.node.Unlock()
	op := work.Operation
	started := s.clock.Now()
	profile := ports.RuntimeProfile{ID: work.Profile.Profile.ID, InboundTag: work.Profile.Profile.InboundTag,
		Method: work.Profile.Profile.Method, BootstrapStatisticsID: work.Profile.Profile.BootstrapStatisticsID}
	statisticsID := work.Identity.StatisticsID
	targetState := "absent"
	if op.DesiredPresence {
		targetState = "present"
	}
	logger := s.logger.With(logging.FieldAllocationID, work.Allocation.ID.String(), logging.FieldOperationID, op.ID.String(),
		logging.FieldTargetState, targetState, logging.FieldNodeID, work.Profile.Profile.InstanceID.String())

	if !op.DesiredPresence || op.Phase == domain.SyncRemoveOld {
		if held, err := s.fence(ctx, work, logger); err != nil || !held {
			return err
		}
		_, err := s.adapter.RemoveUser(ctx, ports.RemoveUserCommand{OperationID: op.ID, ProfileTag: profile.InboundTag, StatisticsID: statisticsID})
		if err != nil {
			kind, retryable := describe(err)
			switch {
			case kind == ports.ErrorUserNotFound:
				// 删除不存在视为收敛。
			case retryable && kind != ports.ErrorInstanceUnavailable:
				if held, err := s.fence(ctx, work, logger); err != nil || !held {
					return err
				}
				present, observeErr := s.observe(ctx, profile, statisticsID)
				if observeErr != nil || present {
					return s.retry(ctx, work, err, logger, started)
				}
			default:
				return s.retry(ctx, work, err, logger, started)
			}
		}
		if !op.DesiredPresence {
			return s.confirm(ctx, work, false, 0, logger, started)
		}
		if err := s.store.AdvancePhase(ctx, op.ID, s.owner, domain.SyncAddDesired, s.clock.Now()); err != nil {
			var invalid *domain.InvalidStateError
			if errors.As(err, &invalid) {
				logger.Warn("synchronization lease lost before rotation add phase; abandoned", logging.FieldResult, "abandoned")
				return nil
			}
			return err
		}
		op.Phase = domain.SyncAddDesired
		work.Operation = op
	}

	key, err := s.keyring.Decrypt(work.Credential.KeyCiphertext, work.Credential.KeyNonce,
		security.SecretAAD("access_credentials", work.Allocation.ID.String(), "user_key", work.Credential.KeyEncryptionVersion))
	if err != nil {
		return s.fail(ctx, work, ports.ErrorInternal, "credential key could not be decrypted", logger, started)
	}
	command := ports.AddUserCommand{OperationID: op.ID, ProfileTag: profile.InboundTag, AllocationID: work.Allocation.ID,
		StatisticsID: statisticsID, CredentialVersion: work.Credential.Version, UserKey: security.NewRedactedString(string(key))}
	if held, err := s.fence(ctx, work, logger); err != nil || !held {
		return err
	}
	if _, err := s.adapter.AddUser(ctx, command); err != nil {
		kind, retryable := describe(err)
		switch {
		case kind == ports.ErrorUserAlreadyExists:
			// 受控修复：先移除再添加，保证 Xray 中的密钥就是期望版本。
			if held, err := s.fence(ctx, work, logger); err != nil || !held {
				return err
			}
			if _, removeErr := s.adapter.RemoveUser(ctx, ports.RemoveUserCommand{OperationID: op.ID, ProfileTag: profile.InboundTag, StatisticsID: statisticsID}); removeErr != nil {
				if removeKind, _ := describe(removeErr); removeKind != ports.ErrorUserNotFound {
					return s.retry(ctx, work, removeErr, logger, started)
				}
			}
			if held, err := s.fence(ctx, work, logger); err != nil || !held {
				return err
			}
			if _, addErr := s.adapter.AddUser(ctx, command); addErr != nil {
				return s.retry(ctx, work, addErr, logger, started)
			}
		case retryable && kind != ports.ErrorInstanceUnavailable:
			if held, err := s.fence(ctx, work, logger); err != nil || !held {
				return err
			}
			present, observeErr := s.observe(ctx, profile, statisticsID)
			if observeErr != nil || !present {
				return s.retry(ctx, work, err, logger, started)
			}
		default:
			return s.retry(ctx, work, err, logger, started)
		}
	}
	return s.confirm(ctx, work, true, work.Credential.Version, logger, started)
}

func (s *Synchronizer) observe(ctx context.Context, profile ports.RuntimeProfile, statisticsID string) (bool, error) {
	users, err := s.adapter.ListUsers(ctx, profile)
	if err != nil {
		return false, err
	}
	for _, user := range users {
		if user.StatisticsID == statisticsID {
			return user.Present, nil
		}
	}
	return false, nil
}

func (s *Synchronizer) confirm(ctx context.Context, work *ports.SyncWork, present bool, version int64, logger *slog.Logger, started time.Time) error {
	now := s.clock.Now()
	confirmed, err := s.store.ConfirmSync(ctx, work.Operation.ID, s.owner, work.Operation.DesiredRevision, version, present, now)
	if err != nil {
		return err
	}
	s.recordSuccess(ctx, work, now)
	if !confirmed {
		logger.Info("synchronization result discarded: superseded or lease lost", logging.FieldResult, "superseded", logging.FieldDurationMS, now.Sub(started).Milliseconds())
		return nil
	}
	logger.Info("synchronization confirmed", logging.FieldResult, "succeeded", logging.FieldDurationMS, now.Sub(started).Milliseconds())
	return s.audit(ctx, work, domain.ActionSyncSucceeded, domain.AuditSucceeded, "Xray projection confirmed", now)
}

func (s *Synchronizer) recordSuccess(ctx context.Context, work *ports.SyncWork, now time.Time) {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
	if err := s.store.MarkInstanceHealthy(ctx, "", now); err != nil {
		s.logger.Warn("record instance health", logging.FieldErrorKind, "internal")
	}
	if work.Profile.Profile.Compatibility == domain.CompatibilityUnreachable && s.onProfileRecovered != nil {
		s.onProfileRecovered(work.Profile.Profile.ID)
	}
}

func (s *Synchronizer) retry(ctx context.Context, work *ports.SyncWork, cause error, logger *slog.Logger, started time.Time) error {
	kind, retryable := describe(cause)
	summary := summaryOf(cause)
	now := s.clock.Now()
	s.trackDrift(ctx, work, kind, summary, now)
	attempts := work.Operation.AttemptCount + 1
	if !retryable || (kind == ports.ErrorInternal && attempts >= maxInternalAttempts) {
		return s.fail(ctx, work, kind, summary, logger, started)
	}
	next := now.Add(domain.NextBackoff(attempts, s.maxRetry, s.random))
	if err := s.store.RescheduleSync(ctx, work.Operation.ID, s.owner, attempts, next, kind, summary); err != nil {
		return err
	}
	logger.Warn("synchronization retry scheduled", logging.FieldResult, "retry_wait", logging.FieldErrorKind, kind,
		"attempt", attempts, "next_attempt_at", next.Format(time.RFC3339), logging.FieldDurationMS, now.Sub(started).Milliseconds())
	return nil
}

func (s *Synchronizer) fail(ctx context.Context, work *ports.SyncWork, kind, summary string, logger *slog.Logger, started time.Time) error {
	now := s.clock.Now()
	if err := s.store.FailSync(ctx, work.Operation.ID, s.owner, kind, summary, now); err != nil {
		return err
	}
	logger.Error("synchronization failed permanently", logging.FieldResult, "permanent_failed", logging.FieldErrorKind, kind,
		logging.FieldDurationMS, now.Sub(started).Milliseconds())
	return s.audit(ctx, work, domain.ActionSyncFailed, domain.AuditFailed, summary, now)
}

// trackDrift 把 Adapter 错误映射到 profile/实例状态（data-model §Profile compatibility 漂移检测）。
func (s *Synchronizer) trackDrift(ctx context.Context, work *ports.SyncWork, kind, summary string, now time.Time) {
	switch kind {
	case ports.ErrorIncompatibleProfile, ports.ErrorUnsupportedProtocol, ports.ErrorProfileNotFound, ports.ErrorVersionMismatch:
		if err := s.store.SetProfileCompatibility(ctx, work.Profile.Profile.ID, domain.CompatibilityIncompatible, summary, now); err != nil {
			s.logger.Warn("record profile drift", logging.FieldErrorKind, "internal")
		}
	case ports.ErrorInstanceUnavailable, ports.ErrorDeadlineExceeded:
		s.mu.Lock()
		s.failures++
		count := s.failures
		s.mu.Unlock()
		if err := s.store.MarkInstanceUnreachable(ctx, kind, summary, now); err != nil {
			s.logger.Warn("record instance health", logging.FieldErrorKind, "internal")
		}
		if count >= unreachableThreshold && work.Profile.Profile.Compatibility == domain.CompatibilityCompatible {
			if err := s.store.SetProfileCompatibility(ctx, work.Profile.Profile.ID, domain.CompatibilityUnreachable, summary, now); err != nil {
				s.logger.Warn("record profile drift", logging.FieldErrorKind, "internal")
			}
		}
	}
}

func (s *Synchronizer) audit(ctx context.Context, work *ports.SyncWork, action string, result domain.AuditResult, summary string, now time.Time) error {
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	operationID := work.Operation.ID
	return s.store.AppendAudit(ctx, domain.AuditEvent{ID: id, OccurredAt: now, ActorType: domain.ActorSystem,
		TargetType: "user", TargetID: work.User.ID, Action: action, Result: result, OperationID: &operationID, SafeSummary: summary})
}

func describe(err error) (kind string, retryable bool) {
	var adapterErr *ports.AdapterError
	if errors.As(err, &adapterErr) {
		return adapterErr.Kind, adapterErr.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ports.ErrorDeadlineExceeded, true
	}
	return ports.ErrorInternal, true
}

func summaryOf(err error) string {
	var adapterErr *ports.AdapterError
	if errors.As(err, &adapterErr) && adapterErr.SafeSummary != "" {
		return adapterErr.SafeSummary
	}
	return "Xray operation failed"
}
