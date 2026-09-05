package application

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

// hookStore 在读取用户快照或到期周期之后执行一次回调，用于模拟“事务外旧快照 vs 并发管理员意图”的竞争（T129）。
type hookStore struct {
	ports.Store
	onUser      func()
	onDueCycles func()
}

func (h *hookStore) User(ctx context.Context, id domain.ID) (ports.UserRecord, error) {
	record, err := h.Store.User(ctx, id)
	if h.onUser != nil {
		hook := h.onUser
		h.onUser = nil
		hook()
	}
	return record, err
}

func (h *hookStore) DueCycles(ctx context.Context, now time.Time) ([]ports.UserRecord, error) {
	records, err := h.Store.DueCycles(ctx, now)
	if h.onDueCycles != nil {
		hook := h.onDueCycles
		h.onDueCycles = nil
		hook()
	}
	return records, err
}

func opCount(t *testing.T, fixture *featureFixture, allocationID domain.ID) int {
	t.Helper()
	var count int
	if err := fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=?`, allocationID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// 采集读取计数期间管理员禁用了用户：越界事实仍记录，但不得用旧快照的 revision 创建封禁操作。
func TestCollectorCrossingDuringConcurrentDisable(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(100)
	record := presentUser(t, fixture, "Race", &limit)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	fixture.adapter.OnReadTraffic = func() {
		if _, err := users.SetAdminEnabled(context.Background(), SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := newTrafficService(fixture, nil).CollectOnce(context.Background())
	if err != nil || summary.Blocked != 0 {
		t.Fatalf("summary = %#v, %v", summary, err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.QuotaState != domain.QuotaExceeded || after.Allocation.AdminEnabled || after.Allocation.DesiredRevision != 2 {
		t.Fatalf("allocation = %#v", after.Allocation)
	}
	if operationsFor(t, fixture, record.Allocation.ID, "quota_block") != 0 || operationsFor(t, fixture, record.Allocation.ID, "disable") != 1 {
		t.Fatalf("ops block=%d disable=%d", operationsFor(t, fixture, record.Allocation.ID, "quota_block"), operationsFor(t, fixture, record.Allocation.ID, "disable"))
	}
	var audits int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND target_id=?`, domain.ActionQuotaExceeded, record.User.ID.String()).Scan(&audits)
	if audits != 1 {
		t.Fatalf("quota_exceeded audits = %d", audits)
	}
}

// 周期切换读取到期周期后管理员已把配额调高并恢复了访问：切换不得再次创建恢复操作。
func TestRolloverDuringConcurrentQuotaRaise(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(100)
	record := presentUser(t, fixture, "Boundary", &limit)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooked := &hookStore{Store: fixture.store}
	hooked.onDueCycles = func() {
		if _, err := users.UpdateUser(context.Background(), UpdateUserInput{ID: record.User.ID, DisplayName: "Boundary", LimitBytes: nil, ResetDay: 1,
			AdminEnabled: true, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
			t.Fatal(err)
		}
	}
	quota := NewQuotaService(hooked, fixture.clock, nil, nil)
	fixture.clock.Set(record.Cycle.EndsAt.Add(time.Second))
	if count, err := quota.RolloverDue(context.Background(), fixture.clock.Now()); err != nil || count != 1 {
		t.Fatalf("rollover count = %d, %v", count, err)
	}
	if operationsFor(t, fixture, record.Allocation.ID, "quota_restore") != 1 {
		t.Fatalf("restore operations = %d, want exactly one (from the quota raise)", operationsFor(t, fixture, record.Allocation.ID, "quota_restore"))
	}
	var cycleAudits int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND actor_type='system'`, domain.ActionCycleRestored).Scan(&cycleAudits)
	if cycleAudits != 0 {
		t.Fatalf("rollover wrote a restore audit although nothing was restored: %d", cycleAudits)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.QuotaState != domain.QuotaWithinLimit || after.Cycle.AccountedUplinkBytes != 0 || !after.Cycle.StartsAt.Equal(record.Cycle.EndsAt) {
		t.Fatalf("after rollover = %#v cycle=%#v", after.Allocation, after.Cycle)
	}
}

// 手动重置读取快照后管理员禁用了用户：重置清零但不得创建恢复操作。
func TestManualResetDuringConcurrentDisable(t *testing.T) {
	fixture := newFeatureFixture(t)
	auth, _ := NewAuthService(fixture.store, fixture.clock)
	adminID, err := auth.InitializeAdministrator(context.Background(), "admin", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	record := presentUser(t, fixture, "Reset", &limit)
	realUsers := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooked := &hookStore{Store: fixture.store}
	hooked.onUser = func() {
		if _, err := realUsers.SetAdminEnabled(context.Background(), SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: appID(t), ActorID: adminID}); err != nil {
			t.Fatal(err)
		}
	}
	hookedUsers := NewUserService(hooked, fixture.keyring, fixture.clock, nil)
	if _, err := hookedUsers.ResetTraffic(context.Background(), ResetTrafficInput{ID: record.User.ID, RequestID: appID(t), ActorID: adminID}); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes != 0 || after.Allocation.AdminEnabled || after.Allocation.QuotaState != domain.QuotaWithinLimit ||
		operationsFor(t, fixture, record.Allocation.ID, "quota_restore") != 0 {
		t.Fatalf("after reset = %#v restore ops=%d", after.Allocation, operationsFor(t, fixture, record.Allocation.ID, "quota_restore"))
	}
}

// 调低配额读取快照后采集已经越界并封禁：调额事务内重读到 exceeded，不得重复创建封禁操作。
func TestLowerQuotaDuringConcurrentCrossing(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(100)
	record := presentUser(t, fixture, "Lower", &limit)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
	fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	traffic := newTrafficService(fixture, nil)
	hooked := &hookStore{Store: fixture.store}
	hooked.onUser = func() {
		if _, err := traffic.CollectOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	users := NewUserService(hooked, fixture.keyring, fixture.clock, nil)
	lower := int64(50)
	if _, err := users.UpdateUser(context.Background(), UpdateUserInput{ID: record.User.ID, DisplayName: "Lower", LimitBytes: &lower, ResetDay: 1,
		AdminEnabled: true, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.QuotaState != domain.QuotaExceeded || after.Allocation.DesiredRevision != 2 || *after.Policy.LimitBytes != 50 {
		t.Fatalf("after lower = %#v policy=%#v", after.Allocation, after.Policy)
	}
	if operationsFor(t, fixture, record.Allocation.ID, "quota_block") != 1 || opCount(t, fixture, record.Allocation.ID) != 2 {
		t.Fatalf("block ops=%d total ops=%d", operationsFor(t, fixture, record.Allocation.ID, "quota_block"), opCount(t, fixture, record.Allocation.ID))
	}
}
