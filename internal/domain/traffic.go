package domain

import (
	"fmt"
	"time"
)

// 数据模型：TrafficDirectionCursor 是单个方向的采集游标。
//
// 字段：
//
//	Counter：最近确认的绝对计数；首次采集前为 nil
//	Epoch：方向纪元，计数下降建立新基线时递增
type TrafficDirectionCursor struct {
	Counter *int64
	Epoch   int64
}

// TrafficSample 是一次采集中某方向的观测。
type TrafficSample struct {
	Found bool
	Bytes uint64
}

const (
	EventCounterDecrease = "counter_decrease"
	EventMissing         = "missing"
	EventReappeared      = "reappeared"
	EventBoundaryGap     = "boundary_gap"
	EventNodeRestart     = "node_restart"
	EventBaseline        = "baseline"
	EventOverflow        = "overflow"
)

// ContinuityEvent 只记录异常，不记录普通样本（data-model §TrafficContinuityEvent）。
type ContinuityEvent struct {
	Type         string
	Direction    string
	OldCounter   *int64
	NewCounter   *int64
	CounterEpoch int64
	Summary      string
}

// DeltaResult 是方向级 delta 规则的输出。
type DeltaResult struct {
	Delta   int64
	Cursor  TrafficDirectionCursor
	Missing bool
	Events  []ContinuityEvent
}

// ApplySample 实现 data-model §TrafficCursor 的方向级规则 1–5 与 8。
// restartConfirmed 表示本轮观测到已确认的新 boot epoch；wasMissing 表示上一轮该方向缺失。
func ApplySample(direction string, cursor TrafficDirectionCursor, sample TrafficSample, restartConfirmed, wasMissing bool) DeltaResult {
	result := DeltaResult{Cursor: cursor}
	if !sample.Found {
		// 规则 4：不产生 delta、不覆盖历史。只有曾经有计数时才记录 missing。
		result.Missing = true
		if cursor.Counter != nil && !wasMissing {
			result.Events = append(result.Events, ContinuityEvent{Type: EventMissing, Direction: direction, OldCounter: cursor.Counter,
				CounterEpoch: cursor.Epoch, Summary: "counter temporarily missing"})
		}
		return result
	}
	if sample.Bytes > uint64(MaxInt64) {
		// 规则 8：溢出按未确认下降处理，不写入溢出值。
		result.Cursor.Epoch = cursor.Epoch + 1
		result.Events = append(result.Events, ContinuityEvent{Type: EventOverflow, Direction: direction, OldCounter: cursor.Counter,
			CounterEpoch: result.Cursor.Epoch, Summary: "counter exceeded the representable range"})
		return result
	}
	current := int64(sample.Bytes)
	result.Cursor.Counter = &current
	if wasMissing && cursor.Counter != nil {
		result.Events = append(result.Events, ContinuityEvent{Type: EventReappeared, Direction: direction, OldCounter: cursor.Counter,
			NewCounter: &current, CounterEpoch: cursor.Epoch, Summary: "counter reappeared"})
	}
	switch {
	case cursor.Counter == nil:
		// 规则 5：首个样本，受管身份加入 Xray 时计数从零开始。
		result.Delta = current
		result.Events = append(result.Events, ContinuityEvent{Type: EventBaseline, Direction: direction, NewCounter: &current,
			CounterEpoch: cursor.Epoch, Summary: "first sample established the baseline"})
	case restartConfirmed:
		// 规则 2：已确认重启，重启后的绝对值全部属于新纪元。
		result.Delta = current
		result.Cursor.Epoch = cursor.Epoch + 1
		result.Events = append(result.Events, ContinuityEvent{Type: EventNodeRestart, Direction: direction, OldCounter: cursor.Counter,
			NewCounter: &current, CounterEpoch: result.Cursor.Epoch, Summary: "node restarted; counting resumed from the new epoch"})
	case current < *cursor.Counter:
		// 规则 3：未确认重启但计数下降，只建立新基线，优先避免重复计量。
		result.Cursor.Epoch = cursor.Epoch + 1
		result.Events = append(result.Events, ContinuityEvent{Type: EventCounterDecrease, Direction: direction, OldCounter: cursor.Counter,
			NewCounter: &current, CounterEpoch: result.Cursor.Epoch, Summary: "counter decreased without a confirmed restart; new baseline established"})
	default:
		// 规则 1：同一纪元且不下降。
		result.Delta = current - *cursor.Counter
	}
	return result
}

// CapabilityGenerationTolerance 是判定「能力世代是否应当前进」时对 boot epoch 差值的容差。
//
// uptime 是 uint32 整秒，boot epoch = 观测时刻截断到秒 − uptime，因此同一个进程的两次观测最多相差
// 一秒；`RestartConfirmed` 用的是「严格大于」，所以一秒容差既能吸收全部量化抖动，
// 又能识别出「上一次启动之后仅隔两秒的重启」。不要改用采集/协调间隔——那会把十几秒内的重启放过去。
const CapabilityGenerationTolerance = time.Second

// RestartConfirmed 判定观测到的 boot epoch 是否代表已确认的重启：与存储值相差超过一个采集间隔。
func RestartConfirmed(stored, observed time.Time, known bool, interval time.Duration) bool {
	if !known || stored.IsZero() {
		return false
	}
	difference := observed.Sub(stored)
	if difference < 0 {
		difference = -difference
	}
	return difference > interval
}

// BoundaryGap 判定两次采集间隔是否超过预期的一个采集周期以上（记录 boundary_gap 事件）。
func BoundaryGap(previous, current time.Time, interval time.Duration) bool {
	if previous.IsZero() || interval <= 0 {
		return false
	}
	return current.Sub(previous) > 2*interval
}

// AddDelta 累加并检查溢出；溢出时返回错误而不是写入错误值。
func AddDelta(base, delta int64) (int64, error) {
	total, overflow := addBytes(base, delta)
	if overflow {
		return 0, fmt.Errorf("traffic aggregate overflow")
	}
	return total, nil
}
