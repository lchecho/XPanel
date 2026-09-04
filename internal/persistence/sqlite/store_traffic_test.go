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
	if ok, err := store.ConfirmSync(context.Background(), record.Operation.ID, 1, 1, true, now); err != nil || !ok {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
	return record
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
	if err := store.CommitTrafficBatch(ctx, batch); err != nil {
		t.Fatal(err)
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
	if err := store.CommitTrafficBatch(ctx, failing); err == nil {
		t.Fatal("duplicate event was accepted")
	}
	loaded, _ = store.User(ctx, present.User.ID)
	if loaded.Cycle.AccountedUplinkBytes != 1000 {
		t.Fatalf("partial batch was committed: %d", loaded.Cycle.AccountedUplinkBytes)
	}

	// 首次越界写 quota_block 操作并把状态置为 exceeded。
	block := domain.NewSynchronizationOperation(fixtureID(t), present.Allocation.ID, 2, false, nil, domain.SyncQuotaBlock, domain.SyncRemoveOld, observed)
	auditID := fixtureID(t)
	exceeded := ports.TrafficBatch{ObservedAt: observed.Add(5 * time.Second), Updates: []ports.TrafficUpdate{{AllocationID: present.Allocation.ID,
		Cursor: batch.Updates[0].Cursor, MarkExceeded: true, QuotaBlock: &block,
		Audit: &domain.AuditEvent{ID: auditID, OccurredAt: observed, ActorType: domain.ActorSystem, TargetType: "user", TargetID: present.User.ID,
			Action: domain.ActionQuotaExceeded, Result: domain.AuditAccepted, SafeSummary: "quota exceeded"}}}}
	if err := store.CommitTrafficBatch(ctx, exceeded); err != nil {
		t.Fatal(err)
	}
	loaded, _ = store.User(ctx, present.User.ID)
	if loaded.Allocation.QuotaState != domain.QuotaExceeded || loaded.Allocation.DesiredRevision != 2 || loaded.Allocation.ProjectionState != domain.ProjectionPending {
		t.Fatalf("allocation after block = %#v", loaded.Allocation)
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
	if err := store.CommitTrafficBatch(ctx, ports.TrafficBatch{ObservedAt: observed, Updates: []ports.TrafficUpdate{{AllocationID: record.Allocation.ID,
		Cursor:      ports.TrafficCursorRecord{AllocationID: record.Allocation.ID, UplinkCounter: &up, DownlinkCounter: &down, LastObservedAt: &observed},
		UplinkDelta: 300, DownlinkDelta: 700, DayStartUTC: now.Truncate(24 * time.Hour), LocalDate: "2026-09-04", Timezone: "UTC"}}}); err != nil {
		t.Fatal(err)
	}
	restore := domain.NewSynchronizationOperation(fixtureID(t), record.Allocation.ID, 2, true, nil, domain.SyncQuotaRestore, domain.SyncAddDesired, now)
	actor := fixtureID(t)
	commandID := fixtureID(t)
	reset := ports.QuotaResetRecord{AllocationID: record.Allocation.ID, CycleID: record.Cycle.ID, EventID: fixtureID(t), ActorID: actor,
		Command: domain.DomainCommand{ID: commandID, ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionTrafficReset,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: domain.Fingerprint("reset", record.User.ID.String()),
			State: domain.CommandCompleted, CreatedAt: now, CompletedAt: &now},
		Operation: &restore, Audit: domain.AuditEvent{ID: fixtureID(t), OccurredAt: now, ActorType: domain.ActorAdministrator, ActorID: &actor,
			TargetType: "user", TargetID: record.User.ID, Action: domain.ActionTrafficReset, Result: domain.AuditSucceeded}, Now: now}
	// 重置事件的 actor 必须是已存在的管理员。
	if _, err := store.db.Write.Exec(`INSERT INTO administrators(id,username,password_hash,password_version,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		actor.String(), "admin", "hash", 1, millis(now), millis(now)); err != nil {
		t.Fatal(err)
	}
	if replay, err := store.ResetCycleTraffic(ctx, reset); err != nil || replay {
		t.Fatalf("reset = %v, %v", replay, err)
	}
	if replay, err := store.ResetCycleTraffic(ctx, reset); err != nil || !replay {
		t.Fatalf("reset replay = %v, %v", replay, err)
	}
	loaded, _ := store.User(ctx, record.User.ID)
	if loaded.Cycle.AccountedUplinkBytes != 0 || loaded.Cycle.GrossUplinkBytes != 300 || loaded.Cycle.ManualResetCount != 1 || !loaded.Cycle.EndsAt.Equal(record.Cycle.EndsAt) {
		t.Fatalf("cycle after reset = %#v", loaded.Cycle)
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

	newCycle := ports.QuotaCycleRecord{ID: fixtureID(t), AllocationID: record.Allocation.ID, StartsAt: record.Cycle.EndsAt,
		EndsAt: record.Cycle.EndsAt.AddDate(0, 1, 0), Timezone: "UTC", ResetDay: 1}
	rolloverOp := domain.NewSynchronizationOperation(fixtureID(t), record.Allocation.ID, 3, true, nil, domain.SyncQuotaRestore, domain.SyncAddDesired, now)
	rollover := ports.CycleRollover{AllocationID: record.Allocation.ID, OldCycleID: record.Cycle.ID, NewCycle: newCycle, Operation: &rolloverOp, Now: now.Add(time.Hour)}
	if err := store.RolloverCycle(ctx, rollover); err != nil {
		t.Fatal(err)
	}
	if err := store.RolloverCycle(ctx, rollover); err != nil {
		t.Fatalf("second rollover failed: %v", err)
	}
	var open, closed, operations int
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='open'`, record.Allocation.ID.String()).Scan(&open)
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='closed'`, record.Allocation.ID.String()).Scan(&closed)
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND desired_revision=3`, record.Allocation.ID.String()).Scan(&operations)
	if open != 1 || closed != 1 || operations != 1 {
		t.Fatalf("cycles open=%d closed=%d ops=%d", open, closed, operations)
	}
	due, _ := store.DueCycles(ctx, newCycle.EndsAt)
	if len(due) != 1 || due[0].Cycle.ID != newCycle.ID {
		t.Fatalf("due cycles = %#v", due)
	}
	next, _ := store.NextCycleEnd(ctx)
	if next == nil || !next.Equal(newCycle.EndsAt) {
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
	disable := domain.NewSynchronizationOperation(fixtureID(t), record.Allocation.ID, 2, false, nil, domain.SyncDisable, domain.SyncRemoveOld, now)
	update := ports.UserUpdateRecord{UserID: record.User.ID, ExpectedRevision: 5, DisplayName: "Renamed", NormalizedName: "renamed",
		LimitBytes: &limit, ResetDay: 10, AdminEnabled: false, QuotaState: domain.QuotaWithinLimit, Operation: &disable,
		Command: domain.DomainCommand{ID: fixtureID(t), ActorType: domain.ActorAdministrator, ActorID: &actor, CommandType: domain.ActionUserUpdated,
			TargetType: "user", TargetID: record.User.ID, RequestFingerprint: domain.Fingerprint("u"), State: domain.CommandCompleted, CreatedAt: now, CompletedAt: &now},
		Now: now}
	if _, err := store.UpdateUser(ctx, update); err == nil {
		t.Fatal("stale revision accepted")
	}
	update.ExpectedRevision = 0
	if replay, err := store.UpdateUser(ctx, update); err != nil || replay {
		t.Fatalf("update = %v, %v", replay, err)
	}
	loaded, _ := store.User(ctx, record.User.ID)
	if loaded.User.DisplayName != "Renamed" || loaded.User.Revision != 1 || loaded.Allocation.AdminEnabled || *loaded.Policy.LimitBytes != 2048 ||
		loaded.Policy.ResetDay != 10 || loaded.Allocation.DesiredRevision != 2 || loaded.Allocation.ProjectionState != domain.ProjectionPending {
		t.Fatalf("loaded after update = %#v policy=%#v", loaded.Allocation, loaded.Policy)
	}
	var superseded int
	_ = store.db.Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND desired_revision=1 AND state='succeeded'`, record.Allocation.ID.String()).Scan(&superseded)
	if superseded != 1 {
		t.Fatal("succeeded create operation must not be rewritten")
	}
	if replay, err := store.UpdateUser(ctx, update); err != nil || !replay {
		t.Fatalf("replayed update = %v, %v", replay, err)
	}
}
