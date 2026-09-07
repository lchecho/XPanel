package worker

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func (f *syncFixture) trafficService(notify func()) *application.TrafficService {
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	return application.NewTrafficService(f.store, f.adapter, f.clock, target, 5*time.Second, notify, nil)
}

func TestCollectorBlocksCrossingWithinOneRoundAndSynchronizerRemoves(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	limit := int64(500)
	id, _, err := f.users.CreateUser(ctx, application.CreateUserInput{DisplayName: "Quota", TemplateID: f.template, LimitBytes: &limit,
		ResetDay: 1, RequestID: fixtureID(t), ActorID: fixtureID(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	record, _ := f.store.User(ctx, id)
	collector := NewCollector(f.trafficService(f.sync.Wake), 5*time.Second, nil)
	f.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 250)
	f.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 250)
	collector.RunOnce(ctx)
	after, _ := f.store.User(ctx, id)
	if after.Allocation.QuotaState != domain.QuotaExceeded || after.Allocation.ProjectionState != domain.ProjectionPending {
		t.Fatalf("allocation after crossing = %#v", after.Allocation)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ = f.store.User(ctx, id)
	if after.Allocation.ProjectionState != domain.ProjectionAbsent || after.Allocation.DisplayState(after.User) != domain.DisplayQuotaExceeded {
		t.Fatalf("allocation after removal = %#v", after.Allocation)
	}
	if present := f.userPresent(record); present {
		t.Fatal("blocked user is still present in fake Xray")
	}
	// 移除后不再采集该用户，历史保留。
	f.clock.Advance(5 * time.Second)
	collector.RunOnce(ctx)
	final, _ := f.store.User(ctx, id)
	if final.Cycle.AccountedUplinkBytes != 250 {
		t.Fatalf("history changed after removal: %d", final.Cycle.AccountedUplinkBytes)
	}
}

func TestSchedulerRestoresQuotaBlockedUserAtBoundary(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	limit := int64(100)
	blockedID, _, err := f.users.CreateUser(ctx, application.CreateUserInput{DisplayName: "Blocked", TemplateID: f.template, LimitBytes: &limit,
		ResetDay: 1, RequestID: fixtureID(t), ActorID: fixtureID(t)})
	if err != nil {
		t.Fatal(err)
	}
	disabledID, _, err := f.users.CreateUser(ctx, application.CreateUserInput{DisplayName: "Disabled", TemplateID: f.template, LimitBytes: &limit,
		ResetDay: 1, RequestID: fixtureID(t), ActorID: fixtureID(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.ID{blockedID, disabledID} {
		record, _ := f.store.User(ctx, id)
		f.adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, 100)
		f.adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, 0)
	}
	NewCollector(f.trafficService(f.sync.Wake), 5*time.Second, nil).RunOnce(ctx)
	if _, err := f.users.UpdateUser(ctx, application.UpdateUserInput{ID: disabledID, DisplayName: "Disabled", LimitBytes: &limit, ResetDay: 1,
		AdminEnabled: false, ExpectedRevision: 0, RequestID: fixtureID(t), ActorID: fixtureID(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	quota := application.NewQuotaService(f.store, f.clock, f.sync.Wake, nil)
	scheduler := NewScheduler(quota, f.clock, time.Minute, nil)
	scheduler.RunOnce(ctx)
	blocked, _ := f.store.User(ctx, blockedID)
	if blocked.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatal("rollover happened before the boundary")
	}
	f.clock.Set(blocked.Cycle.EndsAt.Add(time.Second))
	scheduler.RunOnce(ctx)
	if _, err := f.sync.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	blocked, _ = f.store.User(ctx, blockedID)
	disabled, _ := f.store.User(ctx, disabledID)
	if blocked.Allocation.ProjectionState != domain.ProjectionPresent || blocked.Cycle.AccountedUplinkBytes != 0 || blocked.Allocation.DisplayState(blocked.User) != domain.DisplayActive {
		t.Fatalf("blocked user after rollover = %#v cycle=%#v", blocked.Allocation, blocked.Cycle)
	}
	if disabled.Allocation.ProjectionState != domain.ProjectionAbsent || disabled.Allocation.DisplayState(disabled.User) != domain.DisplayDisabled {
		t.Fatalf("manually disabled user after rollover = %#v", disabled.Allocation)
	}
	if present := f.userPresent(disabled); present {
		t.Fatal("manually disabled user was re-added")
	}
	if present := f.userPresent(blocked); !present {
		t.Fatal("quota-blocked user was not restored")
	}
}

func TestSchedulerRunStopsOnCancel(t *testing.T) {
	f := newSyncFixture(t)
	quota := application.NewQuotaService(f.store, f.clock, nil, nil)
	scheduler := NewScheduler(quota, f.clock, 50*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { scheduler.Run(ctx); close(done) }()
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
	collector := NewCollector(f.trafficService(nil), 20*time.Millisecond, nil)
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { collector.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not stop")
	}
}
