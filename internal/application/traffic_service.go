package application

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/logging"
	"xpanel/internal/ports"
)

// 核心函数：TrafficService 执行一轮非破坏性流量采集并把结果写入 SQLite。
//
// 职责：读取投影为 present 的分配计数，按 data-model §TrafficCursor 规则计算增量，单事务提交游标/累计/日聚合/周期，
//
//	并在首次越界时写入 quota_block 操作；不负责调用 Xray 变更用户。
//
// 约束：RPC 永远在写事务之外；采集失败不覆盖已确认历史；配额判定使用 accounted 总和。
// AI-LOCK：不得使用 reset=true 读取计数；不得把缺失计数当作零。
type TrafficService struct {
	store    ports.Store
	adapter  ports.Adapter
	clock    ports.Clock
	target   ports.InstanceTarget
	interval time.Duration
	notify   func()
	logger   *slog.Logger
}

type CollectionSummary struct {
	Targets  int
	Applied  int
	Blocked  int
	Rolled   int // 采集提交内因样本跨越周期边界而结算的周期数
	Restored int // 结算后按最新事实创建的恢复操作数
	Skipped  int
	Events   int
	Duration time.Duration
}

func NewTrafficService(store ports.Store, adapter ports.Adapter, clock ports.Clock, target ports.InstanceTarget,
	interval time.Duration, notify func(), logger *slog.Logger) *TrafficService {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TrafficService{store: store, adapter: adapter, clock: clock, target: target, interval: interval, notify: notify,
		logger: logger.With(logging.FieldComponent, "collector")}
}

// CollectOnce 采集一轮；返回摘要与 RPC 错误。RPC 失败时只更新实例健康状态。
func (s *TrafficService) CollectOnce(ctx context.Context) (CollectionSummary, error) {
	started := s.clock.Now()
	summary := CollectionSummary{}
	targets, err := s.store.CollectionTargets(ctx)
	if err != nil {
		return summary, err
	}
	summary.Targets = len(targets)
	if len(targets) == 0 {
		observation, err := s.adapter.Probe(ctx, s.target)
		if err != nil {
			s.recordFailure(ctx, err)
			return summary, err
		}
		return summary, s.store.MarkInstanceHealthy(ctx, epochString(observation), s.clock.Now())
	}
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.Identity.StatisticsID)
	}
	round, err := s.readAll(ctx, ids)
	if err != nil {
		s.recordFailure(ctx, err)
		return summary, err
	}
	observedAt := round.Observation.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = s.clock.Now()
	}
	samples := make(map[string]map[ports.Direction]domain.TrafficSample, len(ids))
	for _, snapshot := range round.Snapshots {
		if samples[snapshot.StatisticsID] == nil {
			samples[snapshot.StatisticsID] = make(map[ports.Direction]domain.TrafficSample)
		}
		samples[snapshot.StatisticsID][snapshot.Direction] = domain.TrafficSample{Found: snapshot.Found, Bytes: snapshot.Bytes}
	}
	batch := ports.TrafficBatch{ObservedAt: observedAt}
	for _, target := range targets {
		update, skipped, err := s.buildUpdate(target, samples[target.Identity.StatisticsID], round.Observation, observedAt)
		if err != nil {
			return summary, err
		}
		if skipped {
			summary.Skipped++
			continue
		}
		summary.Events += len(update.Events)
		summary.Applied++
		batch.Updates = append(batch.Updates, update)
	}
	result, err := s.store.CommitTrafficBatch(ctx, batch)
	if err != nil {
		return summary, err
	}
	summary.Blocked, summary.Rolled, summary.Restored = result.Blocked, result.Rolled, result.Restored
	if summary.Blocked+summary.Restored > 0 && s.notify != nil {
		s.notify()
	}
	if err := s.store.MarkInstanceHealthy(ctx, epochString(round.Observation), s.clock.Now()); err != nil {
		return summary, err
	}
	summary.Duration = s.clock.Now().Sub(started)
	s.logger.Info("collection round committed", "targets", summary.Targets, "applied", summary.Applied, "blocked", summary.Blocked,
		"rolled", summary.Rolled, "restored", summary.Restored, "skipped", summary.Skipped, "events", summary.Events,
		logging.FieldResult, "succeeded", logging.FieldDurationMS, summary.Duration.Milliseconds())
	return summary, nil
}

// readBatchSize 是 Adapter 契约允许的单次精确查询上限；超过时分批读取并合并为同一轮提交（FR-013/FR-017）。
const readBatchSize = 20

// readAll 分批读取全部身份的计数；任一批失败即整轮失败，避免部分数据被误判为缺失。
func (s *TrafficService) readAll(ctx context.Context, ids []string) (ports.TrafficRound, error) {
	var merged ports.TrafficRound
	for start := 0; start < len(ids); start += readBatchSize {
		end := start + readBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		round, err := s.adapter.ReadTraffic(ctx, ports.TrafficQuery{Target: s.target, StatisticsIDs: ids[start:end]})
		if err != nil {
			return ports.TrafficRound{}, err
		}
		if start == 0 {
			merged.Observation = round.Observation
		}
		merged.Snapshots = append(merged.Snapshots, round.Snapshots...)
	}
	return merged, nil
}

func (s *TrafficService) buildUpdate(target ports.CollectionTarget, samples map[ports.Direction]domain.TrafficSample,
	observation ports.InstanceObservation, observedAt time.Time) (ports.TrafficUpdate, bool, error) {
	cursor := target.Cursor
	if cursor.LastObservedAt != nil && observedAt.Before(*cursor.LastObservedAt) {
		// 规则 7：迟到或乱序响应整体丢弃。
		s.logger.Warn("late traffic sample dropped", logging.FieldAllocationID, target.Allocation.ID.String())
		return ports.TrafficUpdate{}, true, nil
	}
	restart := false
	if cursor.BootEpoch != "" {
		stored, err := time.Parse(time.RFC3339, cursor.BootEpoch)
		if err == nil {
			restart = domain.RestartConfirmed(stored, observation.BootEpoch, observation.BootEpochKnown, s.interval)
		}
	}
	wasMissing := cursor.MissingSince != nil
	uplink := domain.ApplySample(string(ports.Uplink), domain.TrafficDirectionCursor{Counter: cursor.UplinkCounter, Epoch: cursor.UplinkEpoch},
		samples[ports.Uplink], restart, wasMissing)
	downlink := domain.ApplySample(string(ports.Downlink), domain.TrafficDirectionCursor{Counter: cursor.DownlinkCounter, Epoch: cursor.DownlinkEpoch},
		samples[ports.Downlink], restart, wasMissing)
	events := append(append([]domain.ContinuityEvent{}, uplink.Events...), downlink.Events...)
	if restart {
		events = dedupeRestart(events)
	}
	if cursor.LastObservedAt != nil && domain.BoundaryGap(*cursor.LastObservedAt, observedAt, s.interval) {
		events = append(events, domain.ContinuityEvent{Type: domain.EventBoundaryGap, Summary: "collection gap exceeded two intervals"})
	}
	upDelta, downDelta := uplink.Delta, downlink.Delta
	if _, err := domain.AddDelta(target.TotalUplink, upDelta); err != nil {
		upDelta = 0
		events = append(events, domain.ContinuityEvent{Type: domain.EventOverflow, Direction: string(ports.Uplink), Summary: "lifetime total would overflow"})
	}
	if _, err := domain.AddDelta(target.TotalDownlink, downDelta); err != nil {
		downDelta = 0
		events = append(events, domain.ContinuityEvent{Type: domain.EventOverflow, Direction: string(ports.Downlink), Summary: "lifetime total would overflow"})
	}
	if _, err := domain.AddDelta(target.Cycle.AccountedUplinkBytes, upDelta); err != nil {
		upDelta = 0
	}
	if _, err := domain.AddDelta(target.Cycle.AccountedDownlinkBytes, downDelta); err != nil {
		downDelta = 0
	}
	location, err := time.LoadLocation(target.Cycle.Timezone)
	if err != nil {
		location = time.UTC
	}
	dayStart, localDate := domain.DayBounds(observedAt, location)
	newCursor := ports.TrafficCursorRecord{AllocationID: target.Allocation.ID, BootEpoch: cursor.BootEpoch,
		UplinkCounter: uplink.Cursor.Counter, DownlinkCounter: downlink.Cursor.Counter, UplinkEpoch: uplink.Cursor.Epoch,
		DownlinkEpoch: downlink.Cursor.Epoch, LastObservedAt: &observedAt, LastSuccessAt: cursor.LastSuccessAt, MissingSince: cursor.MissingSince}
	if observation.BootEpochKnown {
		newCursor.BootEpoch = epochString(observation)
	}
	if uplink.Missing || downlink.Missing {
		if newCursor.MissingSince == nil {
			missing := observedAt
			newCursor.MissingSince = &missing
		}
	} else {
		newCursor.MissingSince = nil
	}
	if !uplink.Missing || !downlink.Missing {
		success := observedAt
		newCursor.LastSuccessAt = &success
	}
	update := ports.TrafficUpdate{AllocationID: target.Allocation.ID, Cursor: newCursor, UplinkDelta: upDelta, DownlinkDelta: downDelta,
		DayStartUTC: dayStart, LocalDate: localDate, Timezone: target.Cycle.Timezone}
	for _, event := range events {
		id, err := domain.NewID()
		if err != nil {
			return update, false, err
		}
		update.Events = append(update.Events, ports.ContinuityEventRecord{ID: id, AllocationID: target.Allocation.ID, Event: event, OccurredAt: observedAt})
	}
	// 越界判定与封禁操作由 Store 在事务内按最新事实决定；这里只提供 ID 模板（data-model §Write Ordering）。
	opID, err := domain.NewID()
	if err != nil {
		return update, false, err
	}
	auditID, err := domain.NewID()
	if err != nil {
		return update, false, err
	}
	update.QuotaBlock = &domain.SynchronizationOperation{ID: opID, AllocationID: target.Allocation.ID, CreatedAt: observedAt}
	update.Audit = &domain.AuditEvent{ID: auditID, OccurredAt: observedAt, ActorType: domain.ActorSystem, TargetType: "user",
		TargetID: target.User.ID, Action: domain.ActionQuotaExceeded, Result: domain.AuditAccepted, SafeSummary: "accounted usage reached the quota; removal requested"}
	return update, false, nil
}

// dedupeRestart 把两个方向各自的 node_restart 合并为一条全局事件。
func dedupeRestart(events []domain.ContinuityEvent) []domain.ContinuityEvent {
	result := events[:0]
	seen := false
	for _, event := range events {
		if event.Type == domain.EventNodeRestart {
			if seen {
				continue
			}
			seen = true
			event.Direction = ""
		}
		result = append(result, event)
	}
	return result
}

func (s *TrafficService) recordFailure(ctx context.Context, cause error) {
	kind, summary := ports.ErrorInternal, "traffic collection failed"
	var adapterErr *ports.AdapterError
	if errors.As(cause, &adapterErr) {
		kind, summary = adapterErr.Kind, adapterErr.SafeSummary
	}
	if err := s.store.MarkInstanceUnreachable(ctx, kind, summary, s.clock.Now()); err != nil {
		s.logger.Warn("record instance health", logging.FieldErrorKind, "internal")
	}
	s.logger.Warn("collection round failed", logging.FieldResult, "failed", logging.FieldErrorKind, kind)
}

func epochString(observation ports.InstanceObservation) string {
	if !observation.BootEpochKnown {
		return ""
	}
	return observation.BootEpoch.UTC().Format(time.RFC3339)
}
