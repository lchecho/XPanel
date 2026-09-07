package integration

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
	"xpanel/internal/worker"
)

// matrixApp 用可注入故障的 Store 重新装配应用层与 worker（宪章「开发流程与质量门禁」故障矜阵）。
type matrixApp struct {
	*testsupport.App
	fault     *faultStore
	users     *application.UserService
	traffic   *application.TrafficService
	quota     *application.QuotaService
	reconcile *application.ReconciliationService
	sync      *worker.Synchronizer
	profile   domain.ID
	limit     int64
	record    ports.UserRecord
	requestID domain.ID
}

func newMatrixApp(t *testing.T) *matrixApp {
	t.Helper()
	app := testsupport.New(t)
	fault := newFaultStore(app.Store)
	m := &matrixApp{App: app, fault: fault, limit: 1 << 20}
	m.sync = worker.NewSynchronizer(fault, app.Adapter, app.Keyring, app.Clock, nil, app.Node, worker.SynchronizerOptions{
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, RPCTimeout: app.Target.RPCTimeout, Random: func(n int64) int64 { return n - 1 }})
	m.users = application.NewUserService(fault, app.Keyring, app.Clock, m.sync.Wake)
	m.traffic = application.NewTrafficService(fault, app.Adapter, app.Clock, app.Target, 5*time.Second, m.sync.Wake, nil)
	m.quota = application.NewQuotaService(fault, app.Clock, m.sync.Wake, nil)
	m.reconcile = application.NewReconciliationService(fault, app.Adapter, app.Clock, app.Target, app.Node, 15*time.Second, m.sync.Wake, nil, nil)
	m.profile = app.RegisterCompatibleProfile("Primary")
	m.record = app.CreateUser("Subject", m.profile, &m.limit)
	m.drain()
	m.Adapter.Users["managed"]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
	m.requestID = testsupport.NewID(t)
	return m
}

func (m *matrixApp) drain() {
	if _, err := m.sync.Drain(context.Background()); err != nil {
		m.T.Fatal(err)
	}
}

func (m *matrixApp) present() bool {
	_, ok := m.Adapter.Users["managed"][m.record.Identity.StatisticsID]
	return ok
}

func (m *matrixApp) refresh() ports.UserRecord {
	m.record = m.User(m.record.User.ID)
	return m.record
}

func (m *matrixApp) enabled(value bool) {
	if _, err := m.users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: m.record.User.ID, Enabled: value,
		ExpectedRevision: m.refresh().User.Revision, RequestID: testsupport.NewID(m.T), ActorID: m.AdminID}); err != nil {
		m.T.Fatal(err)
	}
}

func (m *matrixApp) blockByTraffic() {
	m.SetTraffic(m.record, uint64(m.limit), 0)
	if _, err := m.traffic.CollectOnce(context.Background()); err != nil {
		m.T.Fatal(err)
	}
}

// change 描述一种影响访问权的变更：前置条件、执行（可重复执行以模拟重复请求）、期望的最终存在状态与操作原因。
type change struct {
	name       string
	storeWrite string
	rpc        string
	reason     string
	audit      string // 变更本身必须留下的审计动作（SC-008）
	present    bool
	uplink     func(m *matrixApp) int64 // 收敛后当前周期应记入的精确上行字节数（SC-007）
	setup      func(m *matrixApp)
	apply      func(m *matrixApp) error
}

func zeroUplink(*matrixApp) int64 { return 0 }

var changes = []change{
	{name: "create", storeWrite: "CreateUser", rpc: "add_user", reason: "create", audit: domain.ActionUserCreated, present: true, uplink: zeroUplink,
		setup: func(m *matrixApp) {
			// 创建的是一个新用户；把矩阵对象切换为新用户。
			m.enabled(false)
			m.drain()
		},
		apply: func(m *matrixApp) error {
			id, _, err := m.users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fresh", ProfileID: m.profile,
				LimitBytes: &m.limit, ResetDay: 1, RequestID: m.requestID, ActorID: m.AdminID})
			if err == nil {
				m.record = m.User(id)
			}
			return err
		}},
	{name: "enable", storeWrite: "UpdateUser", rpc: "add_user", reason: "enable", audit: domain.ActionUserEnabled, present: true, uplink: zeroUplink,
		setup: func(m *matrixApp) { m.enabled(false); m.drain() },
		apply: func(m *matrixApp) error {
			_, err := m.users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: m.record.User.ID, Enabled: true,
				ExpectedRevision: m.record.User.Revision, RequestID: m.requestID, ActorID: m.AdminID})
			return err
		}},
	{name: "disable", storeWrite: "UpdateUser", rpc: "remove_user", reason: "disable", audit: domain.ActionUserDisabled, present: false, uplink: zeroUplink,
		apply: func(m *matrixApp) error {
			_, err := m.users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: m.record.User.ID, Enabled: false,
				ExpectedRevision: m.record.User.Revision, RequestID: m.requestID, ActorID: m.AdminID})
			return err
		}},
	{name: "rotate", storeWrite: "RotateCredential", rpc: "add_user", reason: "rotate", audit: domain.ActionCredentialRotated, present: true, uplink: zeroUplink,
		apply: func(m *matrixApp) error {
			_, err := m.users.RotateCredential(context.Background(), application.LifecycleInput{ID: m.record.User.ID,
				ExpectedRevision: m.record.User.Revision, RequestID: m.requestID, ActorID: m.AdminID})
			return err
		}},
	{name: "delete", storeWrite: "SoftDeleteUser", rpc: "remove_user", reason: "delete", audit: domain.ActionUserDeleted, present: false, uplink: zeroUplink,
		apply: func(m *matrixApp) error {
			_, err := m.users.DeleteUser(context.Background(), application.LifecycleInput{ID: m.record.User.ID,
				ExpectedRevision: m.record.User.Revision, RequestID: m.requestID, ActorID: m.AdminID})
			return err
		}},
	{name: "quota_block", storeWrite: "CommitTrafficBatch", rpc: "remove_user", reason: "quota_block", audit: domain.ActionQuotaExceeded, present: false,
		uplink: func(m *matrixApp) int64 { return m.limit },
		setup:  func(m *matrixApp) { m.SetTraffic(m.record, uint64(m.limit), 0) },
		apply:  func(m *matrixApp) error { _, err := m.traffic.CollectOnce(context.Background()); return err }},
	{name: "cycle_restore", storeWrite: "RolloverCycle", rpc: "add_user", reason: "quota_restore", audit: domain.ActionCycleRestored, present: true, uplink: zeroUplink,
		setup: func(m *matrixApp) {
			m.blockByTraffic()
			m.drain()
			m.Clock.Set(m.refresh().Cycle.EndsAt.Add(time.Second))
		},
		apply: func(m *matrixApp) error {
			_, err := m.quota.RolloverDue(context.Background(), m.Clock.Now())
			return err
		}},
}

type failure struct {
	name string
	run  func(t *testing.T, m *matrixApp, c change)
}

func deadlineFailure(operation string, applied bool) xrayfake.Failure {
	return xrayfake.Failure{Err: &ports.AdapterError{Kind: ports.ErrorDeadlineExceeded, Operation: operation, Retryable: true, SafeSummary: "timed out"}, Applied: applied}
}

// retryAfterBackoff 把时钟推进到最近的重试时刻或租约到期时刻（模拟进程重启后等待），再执行一轮。
func retryAfterBackoff(m *matrixApp) {
	var next sql.NullInt64
	_ = m.Store.DB().Read.QueryRow(`SELECT MAX(CASE WHEN state='retry_wait' THEN next_attempt_at WHEN state='leased' THEN lease_expires_at END)
        FROM synchronization_operations WHERE state IN ('retry_wait','leased')`).Scan(&next)
	if next.Valid && next.Int64 > 0 {
		if at := time.UnixMilli(next.Int64).UTC(); at.After(m.Clock.Now()) {
			m.Clock.Set(at)
		}
	}
	m.drain()
}

var failures = []failure{
	{name: "db_commit_failure", run: func(t *testing.T, m *matrixApp, c change) {
		before := m.refresh()
		presentBefore := m.present()
		m.fault.FailNext(c.storeWrite)
		if err := c.apply(m); err == nil {
			t.Fatal("injected commit failure was not reported")
		}
		after := m.refresh()
		if after.User.Revision != before.User.Revision || after.Allocation.DesiredRevision != before.Allocation.DesiredRevision || m.present() != presentBefore {
			t.Fatalf("failed commit changed state: %#v → %#v", before.Allocation, after.Allocation)
		}
		if err := c.apply(m); err != nil {
			t.Fatal(err)
		}
		m.drain()
	}},
	{name: "rpc_timeout", run: func(t *testing.T, m *matrixApp, c change) {
		if err := c.apply(m); err != nil {
			t.Fatal(err)
		}
		m.Adapter.Failures[c.rpc] = []xrayfake.Failure{deadlineFailure(c.rpc, false)}
		m.drain()
		retryAfterBackoff(m)
	}},
	{name: "crash_after_rpc_success", run: func(t *testing.T, m *matrixApp, c change) {
		if err := c.apply(m); err != nil {
			t.Fatal(err)
		}
		// 变更已在 Xray 生效但响应丢失；随后确认事务失败一次（进程崩溃）→ 租约到期后恢复。
		m.Adapter.Failures[c.rpc] = []xrayfake.Failure{deadlineFailure(c.rpc, true)}
		m.fault.FailNext("ConfirmSync")
		_, _ = m.sync.Drain(context.Background())
		m.Clock.Advance(11 * time.Second)
		m.drain()
		retryAfterBackoff(m)
	}},
	{name: "xray_restart", run: func(t *testing.T, m *matrixApp, c change) {
		if err := c.apply(m); err != nil {
			t.Fatal(err)
		}
		m.Adapter.Restart()
		m.Adapter.Users["managed"]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
		m.drain()
		if _, err := m.reconcile.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.drain()
	}},
	{name: "duplicate_request", run: func(t *testing.T, m *matrixApp, c change) {
		if err := c.apply(m); err != nil {
			t.Fatal(err)
		}
		if err := c.apply(m); err != nil {
			t.Fatalf("duplicate application failed: %v", err)
		}
		m.drain()
	}},
	{name: "reconciler_replay", run: func(t *testing.T, m *matrixApp, c change) {
		if err := c.apply(m); err != nil {
			t.Fatal(err)
		}
		m.drain()
		for i := 0; i < 2; i++ {
			summary, err := m.reconcile.ReconcileOnce(context.Background())
			if err != nil || summary.Drift != 0 {
				t.Fatalf("reconciler replay #%d produced drift: %#v, %v", i, summary, err)
			}
		}
		m.drain()
	}},
}

func TestFailureMatrixConvergesForEveryChangeAndFault(t *testing.T) {
	for _, c := range changes {
		for _, f := range failures {
			c, f := c, f
			t.Run(fmt.Sprintf("%s/%s", c.name, f.name), func(t *testing.T) {
				m := newMatrixApp(t)
				if c.setup != nil {
					c.setup(m)
					m.refresh()
				}
				f.run(t, m, c)
				assertConverged(t, m, c)
			})
		}
	}
}

func assertConverged(t *testing.T, m *matrixApp, c change) {
	t.Helper()
	record := m.refresh()
	if m.present() != c.present {
		t.Fatalf("presence = %v want %v (allocation %#v)", m.present(), c.present, record.Allocation)
	}
	if record.Allocation.PendingSync() {
		t.Fatalf("allocation still pending: %#v", record.Allocation)
	}
	if c.present {
		remote := m.Adapter.Users["managed"][record.Identity.StatisticsID]
		if remote.CredentialVersion != record.Allocation.DesiredCredentialVersion || record.Credential.State != domain.CredentialActive {
			t.Fatalf("credential mismatch remote=%d desired=%d state=%s", remote.CredentialVersion, record.Allocation.DesiredCredentialVersion, record.Credential.State)
		}
	}
	managed := 0
	for id, user := range m.Adapter.Users["managed"] {
		if user.Kind == "managed" && id == record.Identity.StatisticsID {
			managed++
		}
	}
	if managed > 1 {
		t.Fatalf("duplicate identities in Xray: %d", managed)
	}
	if _, kept := m.Adapter.Users["managed"]["bootstrap"]; !kept {
		t.Fatal("bootstrap identity was removed")
	}
	if record.Cycle.AccountedUplinkBytes < 0 || record.Cycle.AccountedDownlinkBytes < 0 {
		t.Fatal("negative traffic recorded")
	}
	// SC-007：流量精确记入一次——重复采集、重启、重试都不得重复计量。
	if want := c.uplink(m); record.Cycle.AccountedUplinkBytes != want || record.Cycle.AccountedDownlinkBytes != 0 {
		t.Fatalf("accounted traffic up=%d down=%d want up=%d down=0", record.Cycle.AccountedUplinkBytes, record.Cycle.AccountedDownlinkBytes, want)
	}
	if c.name == "cycle_restore" {
		var previous int64
		_ = m.Store.DB().Read.QueryRow(`SELECT accounted_uplink_bytes FROM quota_cycles WHERE allocation_id=? AND id<>? ORDER BY starts_at_utc DESC LIMIT 1`,
			record.Allocation.ID.String(), record.Cycle.ID.String()).Scan(&previous)
		if previous != m.limit {
			t.Fatalf("closed cycle accounted %d want %d", previous, m.limit)
		}
	}
	// 操作唯一性：该变更恰好一个成功操作，且分配下没有任何未终结操作。
	var succeeded, open int
	_ = m.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason=? AND state='succeeded'`,
		record.Allocation.ID.String(), c.reason).Scan(&succeeded)
	_ = m.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND state NOT IN ('succeeded','superseded')`,
		record.Allocation.ID.String()).Scan(&open)
	if succeeded != 1 || open != 0 {
		t.Fatalf("operations for %s: succeeded=%d open=%d (want 1/0)", c.reason, succeeded, open)
	}
	// SC-008：变更动作与成功同步各有审计，且审计不含凭证或连接 URI。
	var intent, confirmed, leaks int
	_ = m.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE target_id=? AND action=?`, record.User.ID.String(), c.audit).Scan(&intent)
	_ = m.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events a JOIN synchronization_operations o ON o.id=a.operation_id
        WHERE a.target_id=? AND a.action=? AND a.result='succeeded' AND o.reason=?`, record.User.ID.String(), domain.ActionSyncSucceeded, c.reason).Scan(&confirmed)
	_ = m.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE safe_summary LIKE '%ss://%' OR safe_summary LIKE '%server_key%'`).Scan(&leaks)
	if intent == 0 || confirmed != 1 || leaks != 0 {
		t.Fatalf("audit for %s: intent=%d confirmed_sync=%d leaks=%d (want >=1/1/0)", c.audit, intent, confirmed, leaks)
	}
}
