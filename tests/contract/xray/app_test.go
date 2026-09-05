package xray_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	xrayadapter "xpanel/internal/adapter/xray"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

// switchableAdapter 让同一个应用在运行中切换底层 Xray 客户端（例如先用极短超时的客户端制造不确定结果）。
type switchableAdapter struct {
	mu    sync.Mutex
	inner ports.Adapter
}

func (s *switchableAdapter) use(adapter ports.Adapter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner = adapter
}

func (s *switchableAdapter) current() ports.Adapter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner
}

func (s *switchableAdapter) Probe(ctx context.Context, target ports.InstanceTarget) (ports.InstanceObservation, error) {
	return s.current().Probe(ctx, target)
}
func (s *switchableAdapter) ValidateProfile(ctx context.Context, profile ports.RuntimeProfile) (ports.ProfileCapabilities, error) {
	return s.current().ValidateProfile(ctx, profile)
}
func (s *switchableAdapter) ListUsers(ctx context.Context, profile ports.RuntimeProfile) ([]ports.RemoteUser, error) {
	return s.current().ListUsers(ctx, profile)
}
func (s *switchableAdapter) AddUser(ctx context.Context, command ports.AddUserCommand) (ports.MutationReceipt, error) {
	return s.current().AddUser(ctx, command)
}
func (s *switchableAdapter) RemoveUser(ctx context.Context, command ports.RemoveUserCommand) (ports.MutationReceipt, error) {
	return s.current().RemoveUser(ctx, command)
}
func (s *switchableAdapter) ReadTraffic(ctx context.Context, query ports.TrafficQuery) (ports.TrafficRound, error) {
	return s.current().ReadTraffic(ctx, query)
}

// liveApp 用真实 SQLite、真实 Xray 客户端装配完整应用（service + worker），worker 由测试显式推进。
func liveApp(t *testing.T, runtime *liveRuntime) (*testsupport.App, *switchableAdapter, ports.InstanceTarget) {
	t.Helper()
	target := ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	adapter := &switchableAdapter{inner: runtime.client}
	app := testsupport.NewWith(t, testsupport.Options{Adapter: adapter, Target: &target})
	app.Clock.Set(time.Now().UTC())
	return app, adapter, target
}

func registerLiveProfile(t *testing.T, app *testsupport.App, name, tag, address, serverKey, bootstrap string) domain.ID {
	t.Helper()
	_, portText, _ := net.SplitHostPort(address)
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	id, err := app.Profiles.RegisterProfile(context.Background(), application.ProfileInput{Name: name, InboundTag: tag, PublicHost: "127.0.0.1",
		PublicPort: port, Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: serverKey, BootstrapStatisticsID: bootstrap,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Validator.ValidateNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func managedIDs(t *testing.T, runtime *liveRuntime, tag, bootstrap string) map[string]int {
	t.Helper()
	users, err := runtime.client.ListUsers(context.Background(), ports.RuntimeProfile{InboundTag: tag, Method: security.MethodAES256, BootstrapStatisticsID: bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	for _, user := range users {
		if user.Kind == "managed" {
			ids[user.StatisticsID]++
		}
	}
	return ids
}

// convergeWithRetries 反复推进 synchronizer，把固定时钟推到下一次重试/租约到期时刻，直到分配不再 pending。
func convergeWithRetries(t *testing.T, app *testsupport.App, id domain.ID) ports.UserRecord {
	t.Helper()
	for i := 0; i < 8; i++ {
		_, _ = app.Sync.Drain(context.Background())
		record := app.User(id)
		if !record.Allocation.PendingSync() {
			return record
		}
		var next sql.NullInt64
		_ = app.Store.DB().Read.QueryRow(`SELECT MAX(CASE WHEN state='retry_wait' THEN next_attempt_at WHEN state='leased' THEN lease_expires_at END)
            FROM synchronization_operations WHERE state IN ('retry_wait','leased')`).Scan(&next)
		if next.Valid && next.Int64 > 0 {
			if at := time.UnixMilli(next.Int64).UTC(); at.After(app.Clock.Now()) {
				app.Clock.Set(at)
			}
		}
	}
	t.Fatalf("allocation did not converge: %#v", app.User(id).Allocation)
	return ports.UserRecord{}
}

// 契约门禁 7：真实 Xray 重启后，应用的 reconciler + synchronizer 只恢复 active 用户；禁用与已删除用户保持缺席。
func TestLiveAppRestartReconcilesActiveUsersOnly(t *testing.T) {
	runtime := startRuntime(t)
	bin := contractBinary(t)
	app, _, _ := liveApp(t, runtime)
	profileID := registerLiveProfile(t, app, "Primary", "managed", runtime.inbound, runtime.serverKey, "bootstrap")
	active := app.CreateUser("Active", profileID, nil)
	disabled := app.CreateUser("Disabled", profileID, nil)
	deleted := app.CreateUser("Deleted", profileID, nil)
	app.Drain()
	if ids := managedIDs(t, runtime, "managed", "bootstrap"); len(ids) != 3 {
		t.Fatalf("managed users before restart = %v", ids)
	}
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: false,
		ExpectedRevision: app.User(disabled.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: deleted.User.ID,
		ExpectedRevision: app.User(deleted.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	app.ReconcileOnce() // 记录重启前的 boot epoch
	before, err := app.Store.ManagedInstance(context.Background())
	if err != nil || before.BootEpoch == "" {
		t.Fatalf("instance before restart = %#v, %v", before, err)
	}

	time.Sleep(1100 * time.Millisecond)
	runtime.restart(t, bin)
	if ids := managedIDs(t, runtime, "managed", "bootstrap"); len(ids) != 0 {
		t.Fatalf("dynamic users survived restart: %v", ids)
	}
	summary := app.ReconcileOnce()
	app.Drain()
	after, _ := app.Store.ManagedInstance(context.Background())
	beforeEpoch, _ := time.Parse(time.RFC3339, before.BootEpoch)
	afterEpoch, err := time.Parse(time.RFC3339, after.BootEpoch)
	if err != nil || !afterEpoch.After(beforeEpoch) {
		t.Fatalf("boot epoch not advanced: before=%q after=%q", before.BootEpoch, after.BootEpoch)
	}
	if summary.Drift != 1 {
		t.Fatalf("reconciler drift = %d, want 1 (only the active user)", summary.Drift)
	}
	ids := managedIDs(t, runtime, "managed", "bootstrap")
	if len(ids) != 1 || ids[active.Identity.StatisticsID] != 1 {
		t.Fatalf("managed users after reconciliation = %v", ids)
	}
	for _, record := range []ports.UserRecord{active, disabled, deleted} {
		current := app.User(record.User.ID)
		if current.Allocation.PendingSync() {
			t.Fatalf("allocation still pending after reconciliation: %#v", current.Allocation)
		}
	}
	var audits int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE target_id=? AND action=?`, active.User.ID.String(), domain.ActionSyncSucceeded).Scan(&audits)
	if audits < 2 {
		t.Fatalf("sync audits for the restored user = %d", audits)
	}
}

// 契约门禁 8：极短超时下结果不确定的新增，通过持久操作的读后写收敛，Xray 中恰好一份身份、恰好一个成功操作。
func TestLiveAppUncertainTimeoutConvergesThroughReadAfterWrite(t *testing.T) {
	runtime := startRuntime(t)
	app, adapter, target := liveApp(t, runtime)
	profileID := registerLiveProfile(t, app, "Primary", "managed", runtime.inbound, runtime.serverKey, "bootstrap")
	impatientTarget := target
	impatientTarget.RPCTimeout = 200 * time.Microsecond
	impatient, err := xrayadapter.New(impatientTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()

	adapter.use(impatient)
	user := app.CreateUser("Uncertain", profileID, nil)
	_, _ = app.Sync.Drain(context.Background())
	pending := app.User(user.User.ID)
	if !pending.Allocation.PendingSync() {
		t.Fatalf("impatient client unexpectedly confirmed the mutation: %#v", pending.Allocation)
	}
	var state string
	_ = app.Store.DB().Read.QueryRow(`SELECT state FROM synchronization_operations WHERE allocation_id=? ORDER BY created_at DESC LIMIT 1`, pending.Allocation.ID.String()).Scan(&state)
	if state != "retry_wait" && state != "leased" {
		t.Fatalf("operation after uncertain timeout is %q", state)
	}

	adapter.use(runtime.client)
	record := convergeWithRetries(t, app, user.User.ID)
	ids := managedIDs(t, runtime, "managed", "bootstrap")
	if len(ids) != 1 || ids[record.Identity.StatisticsID] != 1 {
		t.Fatalf("uncertain mutation left %v in Xray", ids)
	}
	var succeeded, open int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason='create' AND state='succeeded'`, record.Allocation.ID.String()).Scan(&succeeded)
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND state NOT IN ('succeeded','superseded')`, record.Allocation.ID.String()).Scan(&open)
	if succeeded != 1 || open != 0 || record.Credential.State != domain.CredentialActive {
		t.Fatalf("operations succeeded=%d open=%d credential=%s", succeeded, open, record.Credential.State)
	}
}

// 契约门禁 9：多个 profile 共享一个实例时统计 ID 全局唯一，bootstrap 身份不进入采集目标；跨 profile 复用 bootstrap ID 判为不兼容。
func TestLiveAppMultipleProfilesKeepStatisticsIDsUniqueAndExcludeBootstrap(t *testing.T) {
	runtime := startRuntime(t)
	app, _, _ := liveApp(t, runtime)
	first := registerLiveProfile(t, app, "First", "managed", runtime.inbound, runtime.serverKey, "bootstrap")
	second := registerLiveProfile(t, app, "Second", secondInboundTag, runtime.inbound2, runtime.serverKey2, secondBootstrap)
	for _, id := range []domain.ID{first, second} {
		record, err := app.Store.Profile(context.Background(), id)
		if err != nil || record.Profile.Compatibility != domain.CompatibilityCompatible {
			t.Fatalf("profile %s = %#v, %v", id, record.Profile, err)
		}
	}
	records := []ports.UserRecord{app.CreateUser("A1", first, nil), app.CreateUser("A2", first, nil), app.CreateUser("B1", second, nil), app.CreateUser("B2", second, nil)}
	app.Drain()
	seen := map[string]int{}
	for id, count := range managedIDs(t, runtime, "managed", "bootstrap") {
		seen[id] += count
	}
	for id, count := range managedIDs(t, runtime, secondInboundTag, secondBootstrap) {
		seen[id] += count
	}
	if len(seen) != 4 {
		t.Fatalf("managed identities across profiles = %v", seen)
	}
	for _, record := range records {
		if seen[record.Identity.StatisticsID] != 1 || record.Identity.StatisticsID[:7] != "xpanel-" {
			t.Fatalf("statistics id %q count=%d", record.Identity.StatisticsID, seen[record.Identity.StatisticsID])
		}
	}
	targets, err := app.Store.CollectionTargets(context.Background())
	if err != nil || len(targets) != 4 {
		t.Fatalf("collection targets = %d, %v", len(targets), err)
	}
	for _, target := range targets {
		if target.Identity.StatisticsID == "bootstrap" || target.Identity.StatisticsID == secondBootstrap {
			t.Fatalf("bootstrap identity entered traffic collection: %#v", target)
		}
	}
	if summary := app.Collect(); summary.Targets != 4 {
		t.Fatalf("collection summary = %#v", summary)
	}
	var samples int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM xray_user_identities WHERE kind='bootstrap'`).Scan(&samples)
	if samples != 2 {
		t.Fatalf("bootstrap identities registered = %d", samples)
	}
	// 第三个 inbound 的 bootstrap 邮箱与第一个 profile 相同：Xray 接受该配置，但统计计数器按邮箱全局共享，
	// 面板必须把它判为不兼容而不是让两个 profile 共用一个 bootstrap 身份。
	conflict, err := app.Profiles.RegisterProfile(context.Background(), application.ProfileInput{Name: "Conflict", InboundTag: thirdInboundTag,
		PublicHost: "127.0.0.1", PublicPort: 1, Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: runtime.serverKey3,
		BootstrapStatisticsID: "bootstrap", RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Validator.ValidateNow(context.Background(), conflict); err != nil {
		t.Fatal(err)
	}
	record, _ := app.Store.Profile(context.Background(), conflict)
	if record.Profile.Compatibility != domain.CompatibilityIncompatible {
		t.Fatalf("conflicting bootstrap profile = %#v", record.Profile)
	}
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM xray_user_identities WHERE kind='bootstrap'`).Scan(&samples)
	if samples != 2 {
		t.Fatalf("bootstrap identities after conflict = %d, want 2", samples)
	}
}
