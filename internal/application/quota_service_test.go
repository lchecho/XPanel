package application

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func TestRolloverRestoresOnlyQuotaBlockedUsersAndCatchesUp(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(100)
	blocked := presentUser(t, fixture, "Blocked", &limit)
	disabled := presentUser(t, fixture, "Disabled", &limit)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	traffic := newTrafficService(fixture, nil)
	for _, record := range []ports.UserRecord{blocked, disabled} {
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
		fixture.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	}
	if _, err := traffic.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := users.UpdateUser(context.Background(), UpdateUserInput{ID: disabled.User.ID, DisplayName: "Disabled", LimitBytes: &limit, ResetDay: 1,
		AdminEnabled: false, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	woken := 0
	quota := NewQuotaService(fixture.store, fixture.clock, func() { woken++ }, nil)
	if count, err := quota.RolloverDue(context.Background(), fixture.clock.Now()); err != nil || count != 0 {
		t.Fatalf("premature rollover = %d, %v", count, err)
	}
	next, err := quota.NextBoundary(context.Background())
	if err != nil || next == nil || !next.Equal(blocked.Cycle.EndsAt) {
		t.Fatalf("next boundary = %v, %v", next, err)
	}
	// 停机跨越三个月：按顺序补做，只保留一个包含 now 的 open 周期。
	fixture.clock.Set(blocked.Cycle.EndsAt.AddDate(0, 2, 10))
	count, err := quota.RolloverDue(context.Background(), fixture.clock.Now())
	if err != nil || count != 6 || woken != 1 {
		t.Fatalf("rollover count = %d, %v woken=%d", count, err, woken)
	}
	for _, record := range []ports.UserRecord{blocked, disabled} {
		var open, closed int
		_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='open'`, record.Allocation.ID.String()).Scan(&open)
		_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='closed'`, record.Allocation.ID.String()).Scan(&closed)
		if open != 1 || closed != 3 {
			t.Fatalf("%s cycles open=%d closed=%d", record.User.DisplayName, open, closed)
		}
		after, _ := fixture.store.User(context.Background(), record.User.ID)
		if after.Cycle.AccountedUplinkBytes != 0 || !after.Cycle.StartsAt.Before(fixture.clock.Now()) || !after.Cycle.EndsAt.After(fixture.clock.Now()) {
			t.Fatalf("%s open cycle = %#v now=%s", record.User.DisplayName, after.Cycle, fixture.clock.Now())
		}
		if after.Allocation.QuotaState != domain.QuotaWithinLimit {
			t.Fatalf("%s quota state = %s", record.User.DisplayName, after.Allocation.QuotaState)
		}
	}
	if operationsFor(t, fixture, blocked.Allocation.ID, "quota_restore") != 1 || operationsFor(t, fixture, disabled.Allocation.ID, "quota_restore") != 0 {
		t.Fatalf("restore ops blocked=%d disabled=%d", operationsFor(t, fixture, blocked.Allocation.ID, "quota_restore"), operationsFor(t, fixture, disabled.Allocation.ID, "quota_restore"))
	}
	afterDisabled, _ := fixture.store.User(context.Background(), disabled.User.ID)
	if afterDisabled.Allocation.DesiredPresent(afterDisabled.User) {
		t.Fatal("manually disabled user became desired present after rollover")
	}
	var audits int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, domain.ActionCycleRestored).Scan(&audits)
	if audits != 1 {
		t.Fatalf("cycle_restored audits = %d", audits)
	}
	// 重复执行不得再次切换。
	if count, err := quota.RolloverDue(context.Background(), fixture.clock.Now()); err != nil || count != 0 {
		t.Fatalf("second rollover = %d, %v", count, err)
	}
	_ = time.Second
}
