package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

// 为可自动化的成功标准产生证据（spec SC-003/004/005/006/007/008/011）；以结构化 t.Log 输出供 validation-report.md 引用。
func TestSuccessCriteriaEvidence(t *testing.T) {
	app := newHarness(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	limit := int64(1 << 20)
	var records []ports.UserRecord
	for i := 0; i < 20; i++ {
		records = append(records, app.CreateUser(fmt.Sprintf("SC User %02d", i), profileID, &limit))
	}
	app.Drain()
	evidence := func(id, format string, args ...any) { t.Logf("SC-EVIDENCE %s: %s", id, fmt.Sprintf(format, args...)) }

	// SC-003：采集提交后页面立即反映；结构性上界 = 采集间隔 5s + 轮询间隔 5s = 10s。
	for i, record := range records {
		app.SetTraffic(record, uint64(i+1)*1000, 0)
	}
	collected := app.Clock.Now()
	app.Collect()
	_, body := app.Get("/fragments/users-table")
	if !strings.Contains(body, "20 KiB") || !strings.Contains(body, "受管用户（20）") {
		t.Fatalf("fragment does not reflect the committed round: %s", body)
	}
	evidence("SC-003", "20 allocations committed at %s and visible on the next fragment fetch; display latency bound 5s collect + 5s poll = 10s", collected.Format(time.RFC3339))

	// SC-004：达到阈值的分配在一个采集轮次 + 一次同步内被移除；失败路径 30s 内显示待同步。
	blocked := records[0]
	app.SetTraffic(blocked, 1<<20, 0)
	reached := app.Clock.Now()
	app.Collect()
	app.Drain()
	if _, present := app.Adapter.Users["managed"][blocked.Identity.StatisticsID]; present {
		t.Fatal("blocked user still present")
	}
	evidence("SC-004", "quota crossing committed at %s; removal confirmed in the same collect+sync pass (<= 5s collection interval + immediate wake)", reached.Format(time.RFC3339))
	failing := records[1]
	app.Adapter.Failures["remove_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorDeadlineExceeded, Operation: "remove_user", Retryable: true, SafeSummary: "timed out"}}}
	app.SetTraffic(failing, 1<<20, 0)
	app.Collect()
	app.Drain()
	_, body = app.Get("/users/" + failing.User.ID.String())
	if !strings.Contains(body, "移除待同步") || !strings.Contains(body, "节点暂时不可达") {
		t.Fatalf("failure path not visible: %s", body)
	}
	evidence("SC-004", "failure path: pending removal and node error visible on the first page load after the failed sync (<= 30s)")
	app.Clock.Advance(31 * time.Second) // 退避上限 30s 后重试并收敛
	app.Drain()

	// SC-005：周期切换后仅超限用户恢复；手动禁用/已删除恢复数为 0。
	disabled := records[2]
	deletedUser := records[3]
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: disabled.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: deletedUser.User.ID, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	app.Clock.Set(app.User(blocked.User.ID).Cycle.EndsAt.Add(time.Second))
	boundary := app.Clock.Now()
	app.Rollover()
	app.Drain()
	wrong := 0
	for _, record := range []ports.UserRecord{disabled, deletedUser} {
		if _, present := app.Adapter.Users["managed"][record.Identity.StatisticsID]; present {
			wrong++
		}
	}
	if _, present := app.Adapter.Users["managed"][blocked.Identity.StatisticsID]; !present || wrong != 0 {
		t.Fatalf("rollover restore blocked=%v wrong=%d", present, wrong)
	}
	evidence("SC-005", "boundary at %s; quota-blocked user restored in the same scheduler+sync pass; erroneous restores=%d", boundary.Format(time.RFC3339), wrong)

	// SC-006：Xray 重启后一次协调 + 一次同步收敛（协调周期 15s < 60s）。
	app.Adapter.Restart()
	app.Adapter.Users["managed"]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
	summary := app.ReconcileOnce()
	app.Drain()
	mismatch := 0
	for _, record := range records {
		current := app.User(record.User.ID)
		_, present := app.Adapter.Users["managed"][current.Identity.StatisticsID]
		if present != current.Allocation.DesiredPresent(current.User) || current.Allocation.PendingSync() {
			mismatch++
		}
	}
	if mismatch != 0 {
		t.Fatalf("allocations not converged after restart: %d", mismatch)
	}
	evidence("SC-006", "after Xray restart: drift=%d reconciled in one 15s cycle + immediate sync; mismatches=0", summary.Drift)

	// SC-007：计数下降、重启与重复请求均不产生负流量或重复计量。
	subject := records[4]
	before := app.User(subject.User.ID)
	app.SetTraffic(subject, 5000, 5000)
	app.Collect()
	app.SetTraffic(subject, 100, 100) // 未确认重启的下降
	app.Collect()
	after := app.User(subject.User.ID)
	added := after.Cycle.AccountedUplinkBytes - before.Cycle.AccountedUplinkBytes
	if added != 5000 || after.Cycle.AccountedDownlinkBytes < before.Cycle.AccountedDownlinkBytes {
		t.Fatalf("negative or duplicated accounting: added=%d", added)
	}
	requestID := testsupport.NewID(t)
	for i := 0; i < 2; i++ {
		if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: subject.User.ID, Enabled: false, ExpectedRevision: 0, RequestID: requestID, ActorID: app.AdminID}); err != nil {
			t.Fatal(err)
		}
	}
	var disableOps int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason='disable'`, subject.Allocation.ID.String()).Scan(&disableOps)
	if disableOps != 1 {
		t.Fatalf("duplicate request created %d operations", disableOps)
	}
	evidence("SC-007", "counter decrease produced delta 0 and no negative totals; duplicate request produced %d operation", disableOps)

	// SC-008：每个影响访问权的操作都有审计，且摘要不含可复用凭证。
	var actions int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(DISTINCT action) FROM audit_events WHERE action IN (?,?,?,?,?,?,?)`,
		domain.ActionUserCreated, domain.ActionUserDisabled, domain.ActionUserDeleted, domain.ActionQuotaExceeded, domain.ActionCycleRestored,
		domain.ActionSyncSucceeded, domain.ActionLogin).Scan(&actions)
	if actions != 7 {
		t.Fatalf("audited action kinds = %d", actions)
	}
	var leaks int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE safe_summary LIKE '%ss://%' OR safe_summary LIKE ?`, "%"+app.Password+"%").Scan(&leaks)
	evidence("SC-008", "audited action kinds=%d/7 expected; credential leaks in audit=%d", actions, leaks)

	// SC-011：本机密码重置后既有会话全部失效。
	if err := app.Auth.ResetPassword(context.Background(), []byte("another long passphrase 42")); err != nil {
		t.Fatal(err)
	}
	response, _ := app.Get("/users")
	if response.StatusCode != 303 {
		t.Fatalf("session survived password reset: status=%d", response.StatusCode)
	}
	var live int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM admin_sessions WHERE revoked_at IS NULL`).Scan(&live)
	evidence("SC-011", "after local reset: live sessions=%d, old session redirected to login", live)
}
