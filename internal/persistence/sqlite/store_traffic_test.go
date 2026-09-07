package sqlite

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func presentUser(t *testing.T, store *Store, now time.Time, name string) ports.UserCreateRecord {
	t.Helper()
	record := userFixture(t, store, now, name)
	if _, _, err := store.CreateUser(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	leaseForTest(t, store, record.Operation.ID, "test")
	if ok, err := store.ConfirmSync(context.Background(), record.Operation.ID, "test", 1, 1, true, now); err != nil || !ok {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
	return record
}

// leaseForTest 把操作直接置为 owner 持有的租约，供不经 worker 的确认测试使用。
func leaseForTest(t *testing.T, store *Store, id domain.ID, owner string) {
	t.Helper()
	if _, err := store.DB().Write.Exec(`UPDATE synchronization_operations SET state='leased',lease_owner=?,lease_expires_at=? WHERE id=?`,
		owner, time.Now().Add(time.Minute).UnixMilli(), id.String()); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionTargetsAndAtomicBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	present := presentUser(t, store, now, "Present")
	pending := userFixture(t, store, now, "Pending")
	if _, _, err := store.CreateUser(ctx, pending); err != nil {
		t.Fatal(err)
	}
	targets, err := store.CollectionTargets(ctx)
	if err != nil || len(targets) != 1 || targets[0].Allocation.ID != present.Allocation.ID || targets[0].Cursor.UplinkCounter != nil {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	up, down := int64(1000), int64(4000)
	observed := now.Add(5 * time.Second)
	eventID := fixtureID(t)
	batch := ports.TrafficBatch{ObservedAt: observed, Updates: []ports.TrafficUpdate{{AllocationID: present.Allocation.ID,
		Cursor: ports.TrafficCursorRecord{AllocationID: present.Allocation.ID, BootEpoch: "epoch-1", UplinkCounter: &up, DownlinkCounter: &down,
			LastObservedAt: &observed, LastSuccessAt: &observed},
		UplinkDelta: 1000, DownlinkDelta: 4000, DayStartUTC: now.Truncate(24 * time.Hour), LocalDate: "2026-09-04", Timezone: "UTC",
		Events: []ports.ContinuityEventRecord{{ID: eventID, AllocationID: present.Allocation.ID, OccurredAt: observed,
			Event: domain.ContinuityEvent{Type: domain.EventBaseline, Direction: "uplink", NewCounter: &up, Summary: "baseline"}}}}}}
	if result, err := store.CommitTrafficBatch(ctx, batch); err != nil || result.Blocked != 0 {
		t.Fatalf("batch = %#v, %v", result, err)
	}
	loaded, err := store.User(ctx, present.User.ID)
	if err != nil || loaded.Cycle.AccountedUplinkBytes != 1000 || loaded.Cycle.GrossDownlinkBytes != 4000 {
		t.Fatalf("cycle after batch = %#v, %v", loaded.Cycle, err)
	}
	targets, _ = store.CollectionTargets(ctx)
	if *targets[0].Cursor.UplinkCounter != 1000 || targets[0].TotalDownlink != 4000 || targets[0].Cursor.BootEpoch != "epoch-1" {
		t.Fatalf("cursor after batch = %#v totals=%d", targets[0].Cursor, targets[0].TotalDownlink)
	}
	var daily int64
	if err := store.db.Read.QueryRow(`SELECT uplink_bytes FROM daily_traffic_aggregates WHERE allocation_id=?`, present.Allocation.ID.String()).Scan(&daily); err != nil || daily != 1000 {
		t.Fatalf("daily = %d, %v", daily, err)
	}

	// 故障注入：重复的事件 ID 触发唯一约束，整批必须回滚。
	failing := batch
	failing.Updates[0].UplinkDelta = 5
	failing.Updates[0].Events[0].ID = eventID
	if _, err := store.CommitTrafficBatch(ctx, failing); err == nil {
		t.Fatal("duplicate event was accepted")
	}
	loaded, _ = store.User(ctx, present.User.ID)
	if loaded.Cycle.AccountedUplinkBytes != 1000 {
		t.Fatalf("partial batch was committed: %d", loaded.Cycle.AccountedUplinkBytes)
	}

	// 首次越界：事务内按最新策略判定，写 quota_block 操作并把状态置为 exceeded。
	if _, err := store.db.Write.Exec(`UPDATE quota_policies SET limit_bytes=4000 WHERE allocation_id=?`, present.Allocation.ID.String()); err != nil {
		t.Fatal(err)
	}
	block := domain.SynchronizationOperation{ID: fixtureID(t), AllocationID: present.Allocation.ID, CreatedAt: observed}
	auditID := fixtureID(t)
	exceeded := ports.TrafficBatch{ObservedAt: observed.Add(5 * time.Second), Updates: []ports.TrafficUpdate{{AllocationID: present.Allocation.ID,
		Cursor: batch.Updates[0].Cursor, QuotaBlock: &block,
		Audit: &domain.AuditEvent{ID: auditID, OccurredAt: observed, ActorType: domain.ActorSystem, TargetType: "user", TargetID: present.User.ID,
			Action: domain.ActionQuotaExceeded, Result: domain.AuditAccepted, SafeSummary: "quota exceeded"}}}}
	if result, err := store.CommitTrafficBatch(ctx, exceeded); err != nil || result.Blocked != 1 {
		t.Fatalf("blocked = %#v, %v", result, err)
	}
	loaded, _ = store.User(ctx, present.User.ID)
	if loaded.Allocation.QuotaState != domain.QuotaExceeded || loaded.Allocation.DesiredRevision != 2 || loaded.Allocation.ProjectionState != domain.ProjectionPending {
		t.Fatalf("allocation after block = %#v", loaded.Allocation)
	}
	var reason string
	_ = store.db.Read.QueryRow(`SELECT reason FROM synchronization_operations WHERE id=?`, block.ID.String()).Scan(&reason)
	if reason != string(domain.SyncQuotaBlock) {
		t.Fatalf("operation reason = %s", reason)
	}
	if targets, _ := store.CollectionTargets(ctx); len(targets) != 0 {
		t.Fatal("pending removal should leave collection targets")
	}
}

func TestRolloverIsIdempotentAndResetKeepsHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := presentUser(t, store, now, "Cycle")
	observed := now.Add(time.Second)
	up, down := int64(300), int64(700)
	if _, err := store.CommitTrafficBatch(ctx, ports.TrafficBatch{ObservedAt: observed, Updates: []ports.TrafficUpdate{{AllocationID: record.Allocation.ID,
		Cursor:      ports.TrafficCursorRecord{AllocationID: record.Allocation.ID, UplinkCounter: &up, DownlinkCounter: &down, LastObservedAt: &observed},
		UplinkDelta: 300, DownlinkDelta: 700, DayStartUTC: now.Truncate(24 * time.Hour), LocalDate: "2026-09-04", Timezone: "UTC"}}}); err != nil {
		t.Fatal(err)
	}
	// 让用户处于配额超限状态，以便重置与切换触发恢复操作。
	if _, err := store.db.Write.Exec(`UPDATE access_allocations SET quota_state='exceeded' WHERE id=?`, record.Allocation.ID.String()); err != nil {
		t.Fatal(err)
	}
	actor := fixtureID(t)
	commandID := fixtureID(t)
	reset := ports.QuotaResetRecord{AllocationID: record.Allocation.ID, CycleID: record.Cycle.ID, EventID: fixtureID(t), ActorID: actor,
		Command: domain.DomainCommand{ID: commandID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionTrafficReset,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: domain.Fingerprint("reset", record.User.ID.String()),
			State: domain.CommandCompleted, CreatedAt: now, CompletedAt: &now},
		OperationTemplate: domain.SynchronizationOperation{ID: fixtureID(t), CreatedAt: now},
		Audit: domain.AuditEvent{ID: fixtureID(t), OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
			TargetType: "user", TargetID: record.User.ID, Action: domain.ActionTrafficReset, Result: domain.AuditSucceeded}, Now: now}
	// 重置事件的 actor 必须是已存在的管理员。
	if _, err := store.db.Write.Exec(`INSERT INTO administrators(id,username,password_hash,password_version,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		actor.String(), "admin", "hash", 1, millis(now), millis(now)); err != nil {
		t.Fatal(err)
	}
	if replay, created, err := store.ResetCycleTraffic(ctx, reset); err != nil || replay || !created {
		t.Fatalf("reset = %v created=%v, %v", replay, created, err)
	}
	if replay, _, err := store.ResetCycleTraffic(ctx, reset); err != nil || !replay {
		t.Fatalf("reset replay = %v, %v", replay, err)
	}
	loaded, _ := store.User(ctx, record.User.ID)
	if loaded.Cycle.AccountedUplinkBytes != 0 || loaded.Cycle.GrossUplinkBytes != 300 || loaded.Cycle.ManualResetCount != 1 || !loaded.Cycle.EndsAt.Equal(record.Cycle.EndsAt) {
		t.Fatalf("cycle after reset = %#v", loaded.Cycle)
	}
	if loaded.Allocation.QuotaState != domain.QuotaWithinLimit || loaded.Allocation.DesiredRevision != 2 {
		t.Fatalf("allocation after reset = %#v", loaded.Allocation)
	}
	var events, previousUplink int64
	if err := store.db.Read.QueryRow(`SELECT count(*),COALESCE(MAX(previous_accounted_uplink),0) FROM quota_reset_events`).Scan(&events, &previousUplink); err != nil || events != 1 || previousUplink != 300 {
		t.Fatalf("reset events = %d prev=%d, %v", events, previousUplink, err)
	}
	var daily int64
	_ = store.db.Read.QueryRow(`SELECT uplink_bytes FROM daily_traffic_aggregates WHERE allocation_id=?`, record.Allocation.ID.String()).Scan(&daily)
	if daily != 300 {
		t.Fatalf("daily aggregate changed by reset: %d", daily)
	}

	// 再次进入超限，验证切换后由事务内事实决定恢复；新周期边界由事务内的 reset_day 与时区计算。
	if _, err := store.db.Write.Exec(`UPDATE access_allocations SET quota_state='exceeded' WHERE id=?`, record.Allocation.ID.String()); err != nil {
		t.Fatal(err)
	}
	rollover := ports.CycleRollover{AllocationID: record.Allocation.ID,
		OperationTemplate: domain.SynchronizationOperation{ID: fixtureID(t), CreatedAt: now}, Now: record.Cycle.EndsAt.Add(time.Hour)}
	if rolled, restored, err := store.RolloverCycle(ctx, rollover); err != nil || rolled != 1 || !restored {
		t.Fatalf("rollover = %v %v, %v", rolled, restored, err)
	}
	if rolled, _, err := store.RolloverCycle(ctx, rollover); err != nil || rolled != 0 {
		t.Fatalf("second rollover = %v, %v", rolled, err)
	}
	var open, closed, operations int
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='open'`, record.Allocation.ID.String()).Scan(&open)
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='closed'`, record.Allocation.ID.String()).Scan(&closed)
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason='quota_restore'`, record.Allocation.ID.String()).Scan(&operations)
	if open != 1 || closed != 1 || operations != 2 {
		t.Fatalf("cycles open=%d closed=%d restore ops=%d", open, closed, operations)
	}
	current, _ := store.User(ctx, record.User.ID)
	_, wantEnd, _ := domain.CycleBounds(record.Cycle.EndsAt, time.UTC, 1)
	if !current.Cycle.StartsAt.Equal(record.Cycle.EndsAt) || !current.Cycle.EndsAt.Equal(wantEnd) || current.Cycle.ResetDay != 1 {
		t.Fatalf("new cycle bounds = %#v", current.Cycle)
	}
	due, _ := store.DueCycles(ctx, current.Cycle.EndsAt)
	if len(due) != 1 || due[0].Cycle.ID != current.Cycle.ID {
		t.Fatalf("due cycles = %#v", due)
	}
	next, _ := store.NextCycleEnd(ctx)
	if next == nil || !next.Equal(current.Cycle.EndsAt) {
		t.Fatalf("next cycle end = %v", next)
	}
}

func TestUpdateUserRevisionGuardAndOperation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := presentUser(t, store, now, "Editable")
	actor := fixtureID(t)
	limit := int64(2048)
	update := ports.UserUpdateRecord{UserID: record.User.ID, ExpectedRevision: 5, DisplayName: "Renamed", NormalizedName: "renamed",
		LimitBytes: &limit, ResetDay: 10, AdminEnabled: false, OperationTemplate: domain.SynchronizationOperation{ID: fixtureID(t), CreatedAt: now},
		Command: domain.DomainCommand{ID: fixtureID(t), ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionUserUpdated,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: domain.Fingerprint("u"), State: domain.CommandCompleted, CreatedAt: now, CompletedAt: &now},
		Now: now}
	if _, _, err := store.UpdateUser(ctx, update); err == nil {
		t.Fatal("stale revision accepted")
	}
	update.ExpectedRevision = 0
	if replay, created, err := store.UpdateUser(ctx, update); err != nil || replay || !created {
		t.Fatalf("update = %v created=%v, %v", replay, created, err)
	}
	loaded, _ := store.User(ctx, record.User.ID)
	if loaded.User.DisplayName != "Renamed" || loaded.User.Revision != 1 || loaded.Allocation.AdminEnabled || *loaded.Policy.LimitBytes != 2048 ||
		loaded.Policy.ResetDay != 10 || loaded.Allocation.DesiredRevision != 2 || loaded.Allocation.ProjectionState != domain.ProjectionPending {
		t.Fatalf("loaded after update = %#v policy=%#v", loaded.Allocation, loaded.Policy)
	}
	var reason string
	_ = store.db.Read.QueryRow(`SELECT reason FROM synchronization_operations WHERE id=?`, update.OperationTemplate.ID.String()).Scan(&reason)
	if reason != string(domain.SyncDisable) {
		t.Fatalf("operation reason = %s", reason)
	}
	var succeeded int
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND desired_revision=1 AND state='succeeded'`, record.Allocation.ID.String()).Scan(&succeeded)
	if succeeded != 1 {
		t.Fatal("succeeded create operation must not be rewritten")
	}
	if replay, _, err := store.UpdateUser(ctx, update); err != nil || !replay {
		t.Fatalf("replayed update = %v, %v", replay, err)
	}
}

// T128：非当前租约持有者或已被更新意图取代的操作，其失败/重排不得改写 allocation。
func TestSyncFailureWritesAreConditional(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := userFixture(t, store, now, "Guarded")
	if _, _, err := store.CreateUser(ctx, record); err != nil {
		t.Fatal(err)
	}
	if work, err := store.LeaseDue(ctx, "worker-a", now, 10*time.Second); err != nil || work == nil {
		t.Fatalf("lease = %#v, %v", work, err)
	}
	// 错误的 owner：无任何效果。
	if err := store.FailSync(ctx, record.Operation.ID, "worker-b", "upstream_rejected", "nope", now); err != nil {
		t.Fatal(err)
	}
	loaded, _ := store.User(ctx, record.User.ID)
	var state string
	_ = store.db.Read.QueryRow(`SELECT state FROM synchronization_operations WHERE id=?`, record.Operation.ID.String()).Scan(&state)
	if state != string(domain.SyncLeased) || loaded.Allocation.ProjectionState != domain.ProjectionPending || loaded.Allocation.LastSyncErrorCode != "" {
		t.Fatalf("wrong owner changed state: op=%s allocation=%#v", state, loaded.Allocation)
	}
	// 更新意图已提交（revision 2）：旧操作只被取代，allocation 不被改回 error。
	if _, err := store.db.Write.Exec(`UPDATE access_allocations SET desired_revision=2,admin_enabled=0 WHERE id=?`, record.Allocation.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.FailSync(ctx, record.Operation.ID, "worker-a", "upstream_rejected", "nope", now); err != nil {
		t.Fatal(err)
	}
	_ = store.db.Read.QueryRow(`SELECT state FROM synchronization_operations WHERE id=?`, record.Operation.ID.String()).Scan(&state)
	loaded, _ = store.User(ctx, record.User.ID)
	if state != string(domain.SyncSuperseded) || loaded.Allocation.ProjectionState != domain.ProjectionPending || loaded.Allocation.LastSyncErrorCode != "" {
		t.Fatalf("superseded failure changed allocation: op=%s allocation=%#v", state, loaded.Allocation)
	}
	// 已被取代的操作不再接受任何条件写：重排不得把它拉回 retry_wait。
	if err := store.RescheduleSync(ctx, record.Operation.ID, "worker-a", 1, now, "internal", "no longer leased"); err != nil {
		t.Fatal(err)
	}
	_ = store.db.Read.QueryRow(`SELECT state FROM synchronization_operations WHERE id=?`, record.Operation.ID.String()).Scan(&state)
	if state != string(domain.SyncSuperseded) {
		t.Fatalf("reschedule revived a superseded operation: %s", state)
	}
}
