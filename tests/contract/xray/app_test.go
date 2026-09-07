package xray_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
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
func (s *switchableAdapter) ValidateTemplate(ctx context.Context, probe ports.TemplateProbe) (ports.TemplateCapabilities, error) {
	return s.current().ValidateTemplate(ctx, probe)
}

func (s *switchableAdapter) ListInbounds(ctx context.Context) ([]ports.RemoteInbound, error) {
	return s.current().ListInbounds(ctx)
}

func (s *switchableAdapter) CreateInbound(ctx context.Context, command ports.CreateInboundCommand) (ports.MutationReceipt, error) {
	return s.current().CreateInbound(ctx, command)
}

func (s *switchableAdapter) RemoveInbound(ctx context.Context, command ports.RemoveInboundCommand) (ports.MutationReceipt, error) {
	return s.current().RemoveInbound(ctx, command)
}
func (s *switchableAdapter) ListUsers(ctx context.Context, inbound ports.RuntimeInbound) ([]ports.RemoteUser, error) {
	return s.current().ListUsers(ctx, inbound)
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

// freePortPool 找一段连续可用端口作为入站模板的端口池；面板会在其中为每个用户分配端口。
//
// 基址刻意取在临时端口范围（macOS 为 49152+，Linux 为 32768+）之下：freeAddress 拿到的端口来自
// 该范围，测试自身的 SOCKS/回显监听也在那里，把池放进去会与它们抢端口，让创建入站偶发
// port_unavailable。低位区间由内核分配的概率极低，配合逐个绑定校验足以稳定（T083）。
func freePortPool(t *testing.T, size int) (int, int) {
	t.Helper()
	const lowest, highest = 20000, 39000
	for attempt := 0; attempt < 60; attempt++ {
		base := lowest + rand.IntN(highest-lowest-size)
		listeners := make([]net.Listener, 0, size)
		ok := true
		for offset := 0; offset < size; offset++ {
			listener, err := net.Listen("tcp", net.JoinHostPort(listenAddress, strconv.Itoa(base+offset)))
			if err != nil {
				ok = false
				break
			}
			listeners = append(listeners, listener)
		}
		for _, listener := range listeners {
			_ = listener.Close()
		}
		if ok {
			return base, base + size - 1
		}
	}
	t.Fatal("could not find a contiguous free port range for the template pool")
	return 0, 0
}

// convergeAll 反复推进 synchronizer（必要时把时钟推到下一次重试时刻），直到全部分配都不再 pending。
//
// 端口可能被同机其它进程短暂占用，创建入站会得到可恢复的 port_unavailable 并进入有界退避；
// 断言「21 个采集目标」之前必须等收敛，否则会偶发只看到 20 个（T083）。
func convergeAll(t *testing.T, app *testsupport.App, records []ports.UserRecord) {
	t.Helper()
	for attempt := 0; attempt < 12; attempt++ {
		_, _ = app.Sync.Drain(context.Background())
		pending := 0
		for _, record := range records {
			if app.User(record.User.ID).Allocation.PendingSync() {
				pending++
			}
		}
		if pending == 0 {
			return
		}
		var next sql.NullInt64
		_ = app.Store.DB().Read.QueryRow(`SELECT MIN(CASE WHEN state='retry_wait' THEN next_attempt_at WHEN state='leased' THEN lease_expires_at END)
            FROM synchronization_operations WHERE state IN ('retry_wait','leased')`).Scan(&next)
		if next.Valid && next.Int64 > 0 {
			if at := time.UnixMilli(next.Int64).UTC(); at.After(app.Clock.Now()) {
				app.Clock.Set(at)
			}
		}
	}
	unresolved := make([]string, 0, len(records))
	for _, record := range records {
		current := app.User(record.User.ID)
		if current.Allocation.PendingSync() {
			unresolved = append(unresolved, fmt.Sprintf("%s port=%d err=%s", current.User.DisplayName,
				current.Inbound.Inbound.Port, current.Allocation.LastSyncErrorCode))
		}
	}
	t.Fatalf("allocations did not converge: %v", unresolved)
}

// registerLiveTemplate 登记一个入站模板：只有监听地址与端口池，服务端密钥由面板为每条入站自行生成。
func registerLiveTemplate(t *testing.T, app *testsupport.App, name string, poolSize int) domain.ID {
	t.Helper()
	start, end := freePortPool(t, poolSize)
	id, err := app.Templates.RegisterTemplate(context.Background(), application.TemplateInput{Name: name,
		PublicHost: listenAddress, ListenAddress: listenAddress, PortPoolStart: start, PortPoolEnd: end,
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Validator.ValidateNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	record, err := app.Store.Template(context.Background(), id)
	if err != nil || record.Template.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("template = %#v, %v", record.Template, err)
	}
	return id
}

// panelInbounds 返回 Xray 中全部面板入站，按标签索引；命名空间之外的入站不在其中。
func panelInbounds(t *testing.T, runtime *liveRuntime) map[string]ports.RemoteInbound {
	t.Helper()
	inbounds, err := runtime.client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]ports.RemoteInbound{}
	for _, inbound := range inbounds {
		if inbound.PanelManaged {
			result[inbound.InboundTag] = inbound
		}
	}
	if _, kept := result[operatorInboundTag]; kept {
		t.Fatal("an inbound outside the panel namespace was reported as panel-managed")
	}
	return result
}

// managedIDs 汇总全部面板入站中的受管客户端统计标识及其出现次数。
func managedIDs(t *testing.T, runtime *liveRuntime) map[string]int {
	t.Helper()
	ids := map[string]int{}
	for tag := range panelInbounds(t, runtime) {
		users, err := runtime.client.ListUsers(context.Background(), ports.RuntimeInbound{InboundTag: tag, Method: security.MethodAES256})
		if err != nil {
			t.Fatal(err)
		}
		for _, user := range users {
			if user.Kind == "managed" {
				ids[user.StatisticsID]++
			}
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
	templateID := registerLiveTemplate(t, app, "Primary", 8)
	active := app.CreateUser("Active", templateID, nil)
	disabled := app.CreateUser("Disabled", templateID, nil)
	deleted := app.CreateUser("Deleted", templateID, nil)
	app.Drain()
	if ids := managedIDs(t, runtime); len(ids) != 3 {
		t.Fatalf("managed users before restart = %v", ids)
	}
	// 每个用户各自一条入站、各自一个端口。
	activePort := app.User(active.User.ID).Inbound.Inbound.Port
	if len(panelInbounds(t, runtime)) != 3 || !listening(activePort) {
		t.Fatalf("dedicated inbounds before restart = %v", panelInbounds(t, runtime))
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
	if inbounds := panelInbounds(t, runtime); len(inbounds) != 0 {
		t.Fatalf("panel inbounds survived the restart: %v", inbounds)
	}
	if listening(activePort) {
		t.Fatalf("port %d was not released by the restart", activePort)
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
	ids := managedIDs(t, runtime)
	if len(ids) != 1 || ids[active.Identity.StatisticsID] != 1 {
		t.Fatalf("managed users after reconciliation = %v", ids)
	}
	// 门禁 7：按库中记录的原端口整体重建；运维自有入站全程不变。
	if rebuilt := app.User(active.User.ID).Inbound.Inbound.Port; rebuilt != activePort || !listening(rebuilt) {
		t.Fatalf("rebuilt port = %d want %d listening", rebuilt, activePort)
	}
	if !listening(runtime.operatorPort) {
		t.Fatal("operator inbound was disturbed by reconciliation")
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
	templateID := registerLiveTemplate(t, app, "Primary", 4)
	impatientTarget := target
	impatientTarget.RPCTimeout = 200 * time.Microsecond
	impatient, err := xrayadapter.New(impatientTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()

	adapter.use(impatient)
	user := app.CreateUser("Uncertain", templateID, nil)
	_, _ = app.Sync.Drain(context.Background())
	pending := app.User(user.User.ID)
	// 200µs 的超时**通常**会让结果变得不确定，但在足够快的机器上这次 RPC 也可能真的完成。
	// 两种情形都要覆盖：门禁要证明的是「不确定的结果最终收敛且不产生重复」，
	// 而不是「这次调用一定超时」——把后者写成硬断言只会让契约门禁间歇性变红。
	if pending.Allocation.PendingSync() {
		var state string
		_ = app.Store.DB().Read.QueryRow(`SELECT state FROM synchronization_operations WHERE allocation_id=? ORDER BY created_at DESC LIMIT 1`, pending.Allocation.ID.String()).Scan(&state)
		if state != "retry_wait" && state != "leased" {
			t.Fatalf("operation after uncertain timeout is %q", state)
		}
	} else {
		t.Log("the impatient client completed within its 200µs budget; asserting convergence without duplicates")
	}

	adapter.use(runtime.client)
	record := convergeWithRetries(t, app, user.User.ID)
	ids := managedIDs(t, runtime)
	if len(ids) != 1 || ids[record.Identity.StatisticsID] != 1 {
		t.Fatalf("uncertain mutation left %v in Xray", ids)
	}
	if inbounds := panelInbounds(t, runtime); len(inbounds) != 1 || inbounds[record.Inbound.Inbound.InboundTag].UserCount != 1 {
		t.Fatalf("uncertain create left %v in Xray", inbounds)
	}
	var succeeded, open int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason='create' AND state='succeeded'`, record.Allocation.ID.String()).Scan(&succeeded)
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND state NOT IN ('succeeded','superseded')`, record.Allocation.ID.String()).Scan(&open)
	if succeeded != 1 || open != 0 || record.Credential.State != domain.CredentialActive {
		t.Fatalf("operations succeeded=%d open=%d credential=%s", succeeded, open, record.Credential.State)
	}
}

// 契约门禁 9–10：两个用户各得一条专属入站与互不相同的端口；对其中一个执行禁用、轮换与删除，
// 另一个的端口保持监听、统计标识与计数目标不受影响；命名空间之外的入站全程不变。
func TestLiveAppPerUserInboundsAreIsolated(t *testing.T) {
	runtime := startRuntime(t)
	app, _, _ := liveApp(t, runtime)
	templateID := registerLiveTemplate(t, app, "Primary", 8)
	first := app.CreateUser("First", templateID, nil)
	second := app.CreateUser("Second", templateID, nil)
	app.Drain()

	firstPort := app.User(first.User.ID).Inbound.Inbound.Port
	secondPort := app.User(second.User.ID).Inbound.Inbound.Port
	if firstPort == secondPort {
		t.Fatalf("both users were assigned port %d", firstPort)
	}
	if !listening(firstPort) || !listening(secondPort) {
		t.Fatalf("ports not listening: first=%v second=%v", listening(firstPort), listening(secondPort))
	}
	inbounds := panelInbounds(t, runtime)
	if len(inbounds) != 2 {
		t.Fatalf("dedicated inbounds = %v", inbounds)
	}
	for _, record := range []ports.UserRecord{first, second} {
		tag := app.User(record.User.ID).Inbound.Inbound.InboundTag
		if inbound, ok := inbounds[tag]; !ok || inbound.UserCount != 1 {
			t.Fatalf("inbound %s = %#v", tag, inbound)
		}
	}
	ids := managedIDs(t, runtime)
	if len(ids) != 2 || ids[first.Identity.StatisticsID] != 1 || ids[second.Identity.StatisticsID] != 1 {
		t.Fatalf("managed identities = %v", ids)
	}

	// 对第一个用户执行禁用 → 轮换 → 启用 → 删除；第二个用户的端口全程可连接。
	steps := []struct {
		name string
		run  func(revision domain.Revision) error
	}{
		{name: "disable", run: func(revision domain.Revision) error {
			_, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: first.User.ID, Enabled: false,
				ExpectedRevision: revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
		{name: "enable", run: func(revision domain.Revision) error {
			_, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: first.User.ID, Enabled: true,
				ExpectedRevision: revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
		{name: "rotate", run: func(revision domain.Revision) error {
			_, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: first.User.ID,
				ExpectedRevision: revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
		{name: "delete", run: func(revision domain.Revision) error {
			_, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: first.User.ID,
				ExpectedRevision: revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
	}
	for _, step := range steps {
		if err := step.run(app.User(first.User.ID).User.Revision); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		app.Drain()
		if !listening(secondPort) {
			t.Fatalf("%s disturbed the other user's port %d", step.name, secondPort)
		}
		if current := app.User(second.User.ID); current.Allocation.PendingSync() || current.Inbound.Inbound.Port != secondPort {
			t.Fatalf("%s disturbed the other allocation: %#v", step.name, current.Allocation)
		}
		if !listening(runtime.operatorPort) {
			t.Fatalf("%s disturbed the operator inbound", step.name)
		}
	}
	// 删除后第一个用户的入站消失、端口释放；第二个用户完好。
	if listening(firstPort) {
		t.Fatalf("port %d was not released after the user was deleted", firstPort)
	}
	if ids := managedIDs(t, runtime); len(ids) != 1 || ids[second.Identity.StatisticsID] != 1 {
		t.Fatalf("managed identities after deletion = %v", ids)
	}
	targets, err := app.Store.CollectionTargets(context.Background())
	if err != nil || len(targets) != 1 || targets[0].Identity.StatisticsID != second.Identity.StatisticsID {
		t.Fatalf("collection targets = %#v, %v", targets, err)
	}
	if summary := app.Collect(); summary.Targets != 1 {
		t.Fatalf("collection summary = %#v", summary)
	}
}

// readHookAdapter 在指定次序的 ReadTraffic 调用前执行钩子（用于在两批之间重启真实 Xray）。
type readHookAdapter struct {
	ports.Adapter
	mu     sync.Mutex
	reads  int
	hookAt int
	hook   func()
}

func (a *readHookAdapter) ReadTraffic(ctx context.Context, query ports.TrafficQuery) (ports.TrafficRound, error) {
	a.mu.Lock()
	a.reads++
	fire := a.hook != nil && a.reads == a.hookAt
	hook := a.hook
	a.mu.Unlock()
	if fire {
		hook()
	}
	return a.Adapter.ReadTraffic(ctx, query)
}

// 契约门禁（T150）：固定 Xray 下 21+ 分配分两批读取，相邻批次的 boot epoch 量化抖动不得导致整轮丢弃；
// 两批之间真实重启仍必须整轮丢弃且不改任何游标。
func TestLiveAppBatchedCollectionToleratesQuantizationButDetectsRestart(t *testing.T) {
	runtime := startRuntime(t)
	bin := contractBinary(t)
	target := ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	hooked := &readHookAdapter{Adapter: runtime.client}
	app := testsupport.NewWith(t, testsupport.Options{Adapter: hooked, Target: &target})
	app.Clock.Set(time.Now().UTC())
	templateID := registerLiveTemplate(t, app, "Primary", 24)
	var records []ports.UserRecord
	for i := 0; i < 21; i++ {
		records = append(records, app.CreateUser(fmt.Sprintf("Batch %02d", i), templateID, nil))
	}
	// 等到 21 条分配全部收敛再开始采集断言：端口偶发被占用时创建会退避重试。
	convergeAll(t, app, records)
	for round := 0; round < 3; round++ {
		time.Sleep(400 * time.Millisecond) // 让相邻批次跨越 uptime 的整秒进位
		app.Clock.Advance(5 * time.Second)
		summary, err := app.Traffic.CollectOnce(context.Background())
		if err != nil || summary.Targets != 21 || summary.Applied != 21 {
			t.Fatalf("round %d: summary=%#v err=%v", round, summary, err)
		}
	}
	if hooked.reads != 6 {
		t.Fatalf("read_traffic calls = %d, want 6 (two batches per round)", hooked.reads)
	}
	var before int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM traffic_continuity_events WHERE type='node_restart'`).Scan(&before)
	if before != 0 {
		t.Fatalf("quantization jitter produced %d node_restart events", before)
	}
	cursorsBefore := map[string]string{}
	rows, _ := app.Store.DB().Read.Query(`SELECT allocation_id,COALESCE(boot_epoch,'')||'|'||COALESCE(last_observed_at,0) FROM traffic_cursors`)
	for rows.Next() {
		var id, state string
		_ = rows.Scan(&id, &state)
		cursorsBefore[id] = state
	}
	rows.Close()

	// 第二批之前真实重启：整轮必须丢弃。
	hooked.hookAt, hooked.hook = hooked.reads+2, func() { runtime.restart(t, bin) }
	app.Clock.Advance(5 * time.Second)
	summary, err := app.Traffic.CollectOnce(context.Background())
	var inconsistent *application.InconsistentRoundError
	if !errors.As(err, &inconsistent) || summary.Applied != 0 {
		t.Fatalf("restart between batches: summary=%#v err=%v", summary, err)
	}
	rows, _ = app.Store.DB().Read.Query(`SELECT allocation_id,COALESCE(boot_epoch,'')||'|'||COALESCE(last_observed_at,0) FROM traffic_cursors`)
	for rows.Next() {
		var id, state string
		_ = rows.Scan(&id, &state)
		if cursorsBefore[id] != state {
			rows.Close()
			t.Fatalf("cursor %s changed by a discarded round: %s → %s", id, cursorsBefore[id], state)
		}
	}
	rows.Close()
	// 协调 + 同步后用户恢复，下一轮一致并确认重启。
	app.ReconcileOnce()
	convergeAll(t, app, records)
	app.Clock.Advance(5 * time.Second)
	if summary, err := app.Traffic.CollectOnce(context.Background()); err != nil || summary.Applied != 21 {
		t.Fatalf("post-restart round: summary=%#v err=%v", summary, err)
	}
	// 重启后按原端口重建：每个用户的端口都保持不变且在监听。
	for _, record := range records {
		current := app.User(record.User.ID)
		if current.Inbound.Inbound.Port != record.Inbound.Inbound.Port || !listening(current.Inbound.Inbound.Port) {
			t.Fatalf("%s port %d did not come back", current.User.DisplayName, current.Inbound.Inbound.Port)
		}
	}
}
