package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

type allocationSnapshot struct {
	accounted int64
	cursor    sql.NullInt64
	observed  sql.NullInt64
}

func snapshotAll(t *testing.T, fixture *featureFixture, records []ports.UserRecord) map[domain.ID]allocationSnapshot {
	t.Helper()
	result := make(map[domain.ID]allocationSnapshot, len(records))
	for _, record := range records {
		var snapshot allocationSnapshot
		if err := fixture.store.DB().Read.QueryRow(`SELECT qc.accounted_uplink_bytes,tc.uplink_counter,tc.last_observed_at FROM quota_cycles qc
            JOIN traffic_cursors tc ON tc.allocation_id=qc.allocation_id WHERE qc.allocation_id=? AND qc.status='open'`, record.Allocation.ID.String()).Scan(
			&snapshot.accounted, &snapshot.cursor, &snapshot.observed); err != nil {
			t.Fatal(err)
		}
		result[record.User.ID] = snapshot
	}
	return result
}

func countRows(t *testing.T, fixture *featureFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := fixture.store.DB().Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// T146：21+ 分配分两批读取，两批之间 Xray 重启：整轮不得提交任何游标/累计/配额变更；
// 下一轮一致后按重启规则记账（无负增量、无重复计量），随后越界仍被正确封禁。
func TestRestartBetweenBatchesDiscardsRoundWithoutMixedEpochs(t *testing.T) {
	fixture := newFeatureFixture(t)
	fixture.adapter.BootEpoch = fixture.clock.Now().Add(-time.Hour)
	limit := int64(500)
	var records []ports.UserRecord
	for i := 0; i < 25; i++ {
		record := presentUser(t, fixture, fmt.Sprintf("Batch %02d", i), &limit)
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
		records = append(records, record)
	}
	service := newTrafficService(fixture, nil)
	if summary, err := service.CollectOnce(context.Background()); err != nil || summary.Applied != 25 {
		t.Fatalf("baseline summary = %#v, %v", summary, err)
	}
	before := snapshotAll(t, fixture, records)
	eventsBefore := countRows(t, fixture, `SELECT count(*) FROM traffic_continuity_events`)
	for _, record := range records {
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 400)
	}
	fixture.clock.Advance(5 * time.Second)
	// 第一批读取后（第二批之前）重启：计数清零、boot epoch 前进。
	fixture.adapter.OnReadTraffic = func() {
		fixture.adapter.OnReadTraffic = func() { fixture.adapter.Restart() }
	}
	summary, err := service.CollectOnce(context.Background())
	var inconsistent *InconsistentRoundError
	if !errors.As(err, &inconsistent) || summary.Applied != 0 || summary.Blocked != 0 || summary.Skipped != 25 {
		t.Fatalf("mixed-epoch round: summary=%#v err=%v", summary, err)
	}
	after := snapshotAll(t, fixture, records)
	for id, was := range before {
		if now := after[id]; now != was {
			t.Fatalf("allocation %s changed by a discarded round: before=%#v after=%#v", id, was, now)
		}
	}
	if events := countRows(t, fixture, `SELECT count(*) FROM traffic_continuity_events`); events != eventsBefore {
		t.Fatalf("continuity events written by a discarded round: %d → %d", eventsBefore, events)
	}
	if blocks := countRows(t, fixture, `SELECT count(*) FROM synchronization_operations WHERE reason='quota_block'`); blocks != 0 {
		t.Fatalf("quota blocks created by a discarded round: %d", blocks)
	}
	instance, _ := fixture.store.ManagedInstance(context.Background())
	if instance.HealthState != "healthy" || instance.BootEpoch != fixture.adapter.BootEpoch.UTC().Format(time.RFC3339) {
		t.Fatalf("instance diagnostics after inconsistent round = %#v", instance)
	}

	// 下一轮：两批同一 epoch，用户已重新加入且计数从 0 开始 → 确认重启，无负增量，accounted 保持 100。
	for _, record := range records {
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 0)
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	}
	fixture.clock.Advance(5 * time.Second)
	summary, err = service.CollectOnce(context.Background())
	if err != nil || summary.Applied != 25 || summary.Blocked != 0 {
		t.Fatalf("post-restart round: summary=%#v err=%v", summary, err)
	}
	if restarts := countRows(t, fixture, `SELECT count(*) FROM traffic_continuity_events WHERE type=?`, domain.EventNodeRestart); restarts != 25 {
		t.Fatalf("node_restart events = %d, want 25", restarts)
	}
	for id, snapshot := range snapshotAll(t, fixture, records) {
		if snapshot.accounted != 100 || !snapshot.cursor.Valid || snapshot.cursor.Int64 != 0 {
			t.Fatalf("post-restart accounting for %s = %#v", id, snapshot)
		}
	}

	// 再增长 450：accounted 550 > 500，全部封禁一次，没有漏掉。
	for _, record := range records {
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 450)
	}
	fixture.clock.Advance(5 * time.Second)
	summary, err = service.CollectOnce(context.Background())
	if err != nil || summary.Blocked != 25 {
		t.Fatalf("exceeding round: summary=%#v err=%v", summary, err)
	}
	var total int64
	_ = fixture.store.DB().Read.QueryRow(`SELECT SUM(accounted_uplink_bytes) FROM quota_cycles WHERE status='open'`).Scan(&total)
	if total != 25*550 {
		t.Fatalf("accounted total = %d, want %d (no duplicate or negative accounting)", total, 25*550)
	}
}

// T146：批次之间观察时间倒退同样视为不一致，整轮丢弃。
func TestObservationOrderRegressionIsInconsistent(t *testing.T) {
	first := ports.InstanceObservation{ObservedAt: time.Unix(100, 0), BootEpoch: time.Unix(1, 0), BootEpochKnown: true}
	later := ports.InstanceObservation{ObservedAt: time.Unix(101, 0), BootEpoch: time.Unix(1, 0), BootEpochKnown: true}
	earlier := ports.InstanceObservation{ObservedAt: time.Unix(99, 0), BootEpoch: time.Unix(1, 0), BootEpochKnown: true}
	if reason := observationMismatch(first, first, later); reason != "" {
		t.Fatalf("consistent batches flagged: %s", reason)
	}
	if reason := observationMismatch(first, later, earlier); reason == "" {
		t.Fatal("regressed observation time not flagged")
	}
	restarted := later
	restarted.BootEpoch = time.Unix(2, 0)
	if reason := observationMismatch(first, first, restarted); reason == "" {
		t.Fatal("boot epoch change not flagged")
	}
}

// T146：重启后计数暂时缺失时，游标不得推进 boot epoch；计数重新出现时仍按“已确认重启”整值记账，越界不得漏封禁。
func TestMissingCountersAfterRestartKeepEpochUntilCountersReturn(t *testing.T) {
	fixture := newFeatureFixture(t)
	fixture.adapter.BootEpoch = fixture.clock.Now().Add(-time.Hour)
	limit := int64(500)
	record := presentUser(t, fixture, "Missing", &limit)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	service := newTrafficService(fixture, nil)
	if _, err := service.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored := fixture.adapter.BootEpoch.UTC().Format(time.RFC3339)
	fixture.clock.Advance(5 * time.Second)
	fixture.adapter.Restart() // 计数消失，boot epoch 前进
	if summary, err := service.CollectOnce(context.Background()); err != nil || summary.Applied != 1 {
		t.Fatalf("missing round: %#v, %v", summary, err)
	}
	var epoch string
	_ = fixture.store.DB().Read.QueryRow(`SELECT COALESCE(boot_epoch,'') FROM traffic_cursors WHERE allocation_id=?`, record.Allocation.ID.String()).Scan(&epoch)
	if epoch != stored {
		t.Fatalf("cursor epoch advanced while counters were missing: %s (stored %s)", epoch, stored)
	}
	if missing := countRows(t, fixture, `SELECT count(*) FROM traffic_continuity_events WHERE type=?`, domain.EventMissing); missing != 2 {
		t.Fatalf("missing events = %d", missing)
	}
	// 计数重新出现（重启后从 0 计到 450）：确认重启，整值 450 计入 → 550 > 500 封禁。
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 450)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	fixture.clock.Advance(5 * time.Second)
	summary, err := service.CollectOnce(context.Background())
	if err != nil || summary.Blocked != 1 {
		t.Fatalf("reappeared round: %#v, %v", summary, err)
	}
	current, _ := fixture.store.User(context.Background(), record.User.ID)
	if current.Cycle.AccountedUplinkBytes != 550 || current.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatalf("after reappearance: cycle=%#v allocation=%#v", current.Cycle, current.Allocation)
	}
	if restarts := countRows(t, fixture, `SELECT count(*) FROM traffic_continuity_events WHERE type=?`, domain.EventNodeRestart); restarts != 1 {
		t.Fatalf("node_restart events = %d, want 1", restarts)
	}
}
