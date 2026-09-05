package application

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

func presentUser(t *testing.T, fixture *featureFixture, name string, limit *int64) ports.UserRecord {
	t.Helper()
	profiles, err := fixture.store.Profiles(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var profileID domain.ID
	if len(profiles) == 0 {
		profileID = registerCompatibleProfile(t, fixture)
	} else {
		profileID = profiles[0].Profile.ID
	}
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	id, _, err := users.CreateUser(context.Background(), CreateUserInput{DisplayName: name, ProfileID: profileID, LimitBytes: limit,
		ResetDay: 1, RequestID: appID(t), ActorID: appID(t)})
	if err != nil {
		t.Fatal(err)
	}
	var operationID string
	if err := fixture.store.DB().Read.QueryRow(`SELECT id FROM synchronization_operations WHERE allocation_id=(SELECT id FROM access_allocations WHERE user_id=?)`, id.String()).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if ok, err := fixture.store.ConfirmIfRevisionCurrent(context.Background(), domain.ID(operationID), 1, 1, true, fixture.clock.Now()); err != nil || !ok {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
	record, err := fixture.store.User(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.adapter.Users["managed"] == nil {
		fixture.adapter.Users["managed"] = map[string]ports.RemoteUser{}
	}
	fixture.adapter.Users["managed"][record.Identity.StatisticsID] = ports.RemoteUser{StatisticsID: record.Identity.StatisticsID, Present: true, Kind: "managed", CredentialVersion: 1}
	return record
}

func newTrafficService(fixture *featureFixture, notify func()) *TrafficService {
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	return NewTrafficService(fixture.store, fixture.adapter, fixture.clock, target, 5*time.Second, notify, nil)
}

func operationsFor(t *testing.T, fixture *featureFixture, allocationID domain.ID, reason string) int {
	t.Helper()
	var count int
	if err := fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason=?`, allocationID.String(), reason).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func eventsFor(t *testing.T, fixture *featureFixture, allocationID domain.ID, eventType string) int {
	t.Helper()
	var count int
	if err := fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM traffic_continuity_events WHERE allocation_id=? AND type=?`, allocationID.String(), eventType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCollectOnceAccountsUsageAndBlocksExactlyAtQuota(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(1000)
	record := presentUser(t, fixture, "Alice", &limit)
	woken := 0
	service := newTrafficService(fixture, func() { woken++ })
	id := record.Identity.StatisticsID
	fixture.adapter.SetCounter(id, ports.Uplink, 300)
	fixture.adapter.SetCounter(id, ports.Downlink, 400)
	summary, err := service.CollectOnce(context.Background())
	if err != nil || summary.Applied != 1 || summary.Blocked != 0 {
		t.Fatalf("summary = %#v, %v", summary, err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes != 300 || after.Cycle.AccountedDownlinkBytes != 400 || after.Allocation.QuotaState != domain.QuotaWithinLimit {
		t.Fatalf("cycle after first round = %#v", after.Cycle)
	}
	if eventsFor(t, fixture, record.Allocation.ID, domain.EventBaseline) != 2 {
		t.Fatal("baseline events missing")
	}
	fixture.clock.Advance(5 * time.Second)
	fixture.adapter.SetCounter(id, ports.Uplink, 600)
	summary, err = service.CollectOnce(context.Background())
	if err != nil || summary.Blocked != 1 || woken != 1 {
		t.Fatalf("crossing summary = %#v, %v woken=%d", summary, err, woken)
	}
	after, _ = fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes+after.Cycle.AccountedDownlinkBytes != 1000 || after.Allocation.QuotaState != domain.QuotaExceeded ||
		after.Allocation.DesiredRevision != 2 || after.Allocation.DisplayState(after.User) != domain.DisplayQuotaDisabling {
		t.Fatalf("allocation at quota = %#v state=%s", after.Allocation, after.Allocation.DisplayState(after.User))
	}
	if operationsFor(t, fixture, record.Allocation.ID, "quota_block") != 1 {
		t.Fatal("quota_block operation missing")
	}
	var audits int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, domain.ActionQuotaExceeded).Scan(&audits)
	if audits != 1 {
		t.Fatalf("quota_exceeded audits = %d", audits)
	}
	// 移除待同步期间用户仍 present，再次采集继续计量但不得重复封禁。
	fixture.clock.Advance(5 * time.Second)
	fixture.adapter.SetCounter(id, ports.Uplink, 700)
	if summary, err = service.CollectOnce(context.Background()); err != nil || summary.Blocked != 0 || summary.Applied != 0 {
		t.Fatalf("post-block summary = %#v, %v", summary, err)
	}
	instance, _ := fixture.store.ManagedInstance(context.Background())
	if instance.HealthState != "healthy" || instance.LastSuccessAt == nil {
		t.Fatalf("instance = %#v", instance)
	}
}

func TestCollectOnceHandlesDecreaseRestartMissingAndFailure(t *testing.T) {
	fixture := newFeatureFixture(t)
	record := presentUser(t, fixture, "Bob", nil)
	service := newTrafficService(fixture, nil)
	id := record.Identity.StatisticsID
	fixture.adapter.SetCounter(id, ports.Uplink, 1000)
	fixture.adapter.SetCounter(id, ports.Downlink, 1000)
	if _, err := service.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 未确认重启的计数下降：无增量、建立新基线。
	fixture.clock.Advance(5 * time.Second)
	fixture.adapter.SetCounter(id, ports.Uplink, 100)
	if _, err := service.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes != 1000 || eventsFor(t, fixture, record.Allocation.ID, domain.EventCounterDecrease) != 1 {
		t.Fatalf("decrease handling: accounted=%d", after.Cycle.AccountedUplinkBytes)
	}
	// 已确认重启：重启后的绝对值计入新纪元。
	fixture.clock.Advance(5 * time.Second)
	fixture.adapter.Restart()
	fixture.adapter.Users["managed"] = map[string]ports.RemoteUser{id: {StatisticsID: id, Present: true, Kind: "managed"}}
	fixture.adapter.SetCounter(id, ports.Uplink, 50)
	fixture.adapter.SetCounter(id, ports.Downlink, 60)
	if _, err := service.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ = fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes != 1050 || after.Cycle.AccountedDownlinkBytes != 1060 || eventsFor(t, fixture, record.Allocation.ID, domain.EventNodeRestart) != 1 {
		t.Fatalf("restart handling: up=%d down=%d", after.Cycle.AccountedUplinkBytes, after.Cycle.AccountedDownlinkBytes)
	}
	// 缺失计数：不产生增量、不覆盖历史，记录 missing 一次。
	fixture.clock.Advance(5 * time.Second)
	fixture.adapter.Counters = map[string]map[ports.Direction]uint64{}
	for i := 0; i < 2; i++ {
		if _, err := service.CollectOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.Advance(5 * time.Second)
	}
	after, _ = fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes != 1050 || eventsFor(t, fixture, record.Allocation.ID, domain.EventMissing) != 2 {
		t.Fatalf("missing handling: accounted=%d missing events=%d", after.Cycle.AccountedUplinkBytes, eventsFor(t, fixture, record.Allocation.ID, domain.EventMissing))
	}
	targets, _ := fixture.store.CollectionTargets(context.Background())
	if targets[0].Cursor.MissingSince == nil || *targets[0].Cursor.UplinkCounter != 50 {
		t.Fatalf("cursor during missing = %#v", targets[0].Cursor)
	}
	// RPC 失败：实例标记不可达，数据不变。
	fixture.adapter.Available = false
	if _, err := service.CollectOnce(context.Background()); err == nil {
		t.Fatal("collection succeeded while Xray is unavailable")
	}
	instance, _ := fixture.store.ManagedInstance(context.Background())
	if instance.HealthState != "unreachable" || instance.LastSuccessAt == nil {
		t.Fatalf("instance after failure = %#v", instance)
	}
}

func TestUpdateUserQuotaTransitions(t *testing.T) {
	fixture := newFeatureFixture(t)
	limit := int64(1000)
	record := presentUser(t, fixture, "Carol", &limit)
	woken := 0
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, func() { woken++ })
	id := record.Identity.StatisticsID
	fixture.adapter.SetCounter(id, ports.Uplink, 400)
	fixture.adapter.SetCounter(id, ports.Downlink, 100)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	lower := int64(500)
	replay, err := users.UpdateUser(context.Background(), UpdateUserInput{ID: record.User.ID, DisplayName: "Carol", LimitBytes: &lower, ResetDay: 1,
		AdminEnabled: true, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)})
	if err != nil || replay || woken != 1 {
		t.Fatalf("lower quota = %v, %v woken=%d", replay, err, woken)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.QuotaState != domain.QuotaExceeded || operationsFor(t, fixture, record.Allocation.ID, "quota_block") != 1 || after.User.Revision != 1 {
		t.Fatalf("after lowering = %#v revision=%d", after.Allocation, after.User.Revision)
	}
	stale := UpdateUserInput{ID: record.User.ID, DisplayName: "Carol", LimitBytes: nil, ResetDay: 1, AdminEnabled: true, ExpectedRevision: 0, RequestID: appID(t), ActorID: appID(t)}
	if _, err := users.UpdateUser(context.Background(), stale); err == nil {
		t.Fatal("stale revision accepted")
	}
	raise := stale
	raise.ExpectedRevision = 1
	if _, err := users.UpdateUser(context.Background(), raise); err != nil || woken != 2 {
		t.Fatalf("raise quota = %v woken=%d", err, woken)
	}
	after, _ = fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.QuotaState != domain.QuotaWithinLimit || operationsFor(t, fixture, record.Allocation.ID, "quota_restore") != 1 || after.Policy.LimitBytes != nil {
		t.Fatalf("after raising = %#v", after.Allocation)
	}
	if replay, err := users.UpdateUser(context.Background(), raise); err != nil || !replay || woken != 2 {
		t.Fatalf("replay = %v, %v woken=%d", replay, err, woken)
	}
	// 手动禁用同时超限：调高配额不得恢复。
	disable := UpdateUserInput{ID: record.User.ID, DisplayName: "Carol", LimitBytes: &lower, ResetDay: 1, AdminEnabled: false, ExpectedRevision: 2, RequestID: appID(t), ActorID: appID(t)}
	if _, err := users.UpdateUser(context.Background(), disable); err != nil {
		t.Fatal(err)
	}
	unlimited := UpdateUserInput{ID: record.User.ID, DisplayName: "Carol", LimitBytes: nil, ResetDay: 1, AdminEnabled: false, ExpectedRevision: 3, RequestID: appID(t), ActorID: appID(t)}
	if _, err := users.UpdateUser(context.Background(), unlimited); err != nil {
		t.Fatal(err)
	}
	after, _ = fixture.store.User(context.Background(), record.User.ID)
	if after.Allocation.DesiredPresent(after.User) || operationsFor(t, fixture, record.Allocation.ID, "quota_restore") != 1 || after.Allocation.DisplayState(after.User) != domain.DisplayDisabling {
		t.Fatalf("manually disabled user was restored: %#v", after.Allocation)
	}
	var audits int
	_ = fixture.store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN (?,?,?)`, domain.ActionUserUpdated, domain.ActionUserDisabled, domain.ActionQuotaExceeded).Scan(&audits)
	if audits < 5 {
		t.Fatalf("audit count = %d", audits)
	}
}

func TestResetTrafficRestoresOnlyWhenQuotaIsTheOnlyBlocker(t *testing.T) {
	fixture := newFeatureFixture(t)
	auth, err := NewAuthService(fixture.store, fixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	adminID, err := auth.InitializeAdministrator(context.Background(), "admin", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(100)
	record := presentUser(t, fixture, "Dave", &limit)
	users := NewUserService(fixture.store, fixture.keyring, fixture.clock, nil)
	id := record.Identity.StatisticsID
	fixture.adapter.SetCounter(id, ports.Uplink, 80)
	fixture.adapter.SetCounter(id, ports.Downlink, 40)
	if _, err := newTrafficService(fixture, nil).CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := fixture.store.User(context.Background(), record.User.ID)
	if before.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatalf("expected exceeded, got %s", before.Allocation.QuotaState)
	}
	input := ResetTrafficInput{ID: record.User.ID, RequestID: appID(t), ActorID: adminID}
	if replay, err := users.ResetTraffic(context.Background(), input); err != nil || replay {
		t.Fatalf("reset = %v, %v", replay, err)
	}
	if replay, err := users.ResetTraffic(context.Background(), input); err != nil || !replay {
		t.Fatalf("reset replay = %v, %v", replay, err)
	}
	after, _ := fixture.store.User(context.Background(), record.User.ID)
	if after.Cycle.AccountedUplinkBytes != 0 || after.Cycle.GrossUplinkBytes != 80 || !after.Cycle.EndsAt.Equal(before.Cycle.EndsAt) ||
		after.Allocation.QuotaState != domain.QuotaWithinLimit || operationsFor(t, fixture, record.Allocation.ID, "quota_restore") != 1 {
		t.Fatalf("after reset cycle=%#v allocation=%#v", after.Cycle, after.Allocation)
	}
	// 手动禁用的用户：重置清零但不恢复。
	if _, err := users.UpdateUser(context.Background(), UpdateUserInput{ID: record.User.ID, DisplayName: "Dave", LimitBytes: &limit, ResetDay: 1,
		AdminEnabled: false, ExpectedRevision: 0, RequestID: appID(t), ActorID: adminID}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.ResetTraffic(context.Background(), ResetTrafficInput{ID: record.User.ID, RequestID: appID(t), ActorID: adminID}); err != nil {
		t.Fatal(err)
	}
	if operationsFor(t, fixture, record.Allocation.ID, "quota_restore") != 1 {
		t.Fatal("reset restored a manually disabled user")
	}
}
