package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

func retagProfile(app *testsupport.App, id domain.ID, tag string) error {
	record, err := app.Store.Profile(context.Background(), id)
	if err != nil {
		return err
	}
	p := record.Profile
	return app.Profiles.UpdateProfile(context.Background(), id, application.ProfileInput{Name: p.Name, InboundTag: tag, PublicHost: p.PublicHost,
		PublicPort: p.PublicPort, Method: p.Method, Network: p.Network, BootstrapStatisticsID: p.BootstrapStatisticsID,
		ExpectedRevision: p.Revision, RequestID: testsupport.NewID(app.T), ActorID: app.AdminID})
}

func managedOnInbound(app *testsupport.App, tag string) []string {
	var ids []string
	for id, user := range app.Adapter.Users[tag] {
		if user.Kind == "managed" && strings.HasPrefix(id, "xpanel-") {
			ids = append(ids, id)
		}
	}
	return ids
}

// T144：删除事务已提交但移除尚未确认时，不得修改 inbound_tag；确认 absent 后才允许，旧入站不遗留可用 xpanel- 身份。
func TestProfileContractFrozenUntilDeletedUserRemovalConfirmed(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	user := app.CreateUser("Leaving", profileID, nil)
	app.Drain()
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: user.User.ID, ExpectedRevision: app.User(user.User.ID).User.Revision,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	var conflict *domain.ConflictError
	if err := retagProfile(app, profileID, "managed-next"); !errors.As(err, &conflict) {
		t.Fatalf("retag with unconfirmed removal err = %v, want conflict", err)
	}
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 1 {
		t.Fatalf("identity vanished without synchronizer: %v", left)
	}
	record, _ := app.Store.Profile(context.Background(), profileID)
	if record.Profile.InboundTag != testsupport.ProfileTag {
		t.Fatalf("inbound tag changed despite conflict: %s", record.Profile.InboundTag)
	}
	app.Drain()
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 0 {
		t.Fatalf("old inbound still has managed identities after removal: %v", left)
	}
	if err := retagProfile(app, profileID, "managed-next"); err != nil {
		t.Fatalf("retag after confirmed absence: %v", err)
	}
	record, _ = app.Store.Profile(context.Background(), profileID)
	if record.Profile.InboundTag != "managed-next" || record.Profile.Compatibility != domain.CompatibilityUnverified {
		t.Fatalf("profile after retag = %#v", record.Profile)
	}
}

// T144：未知身份的移除意图已排队时不得改 tag；synchronizer 执行完毕后才允许，旧入站不遗留 xpanel- 身份。
func TestProfileContractFrozenWhileDriftRemovalQueued(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	const unknown = "xpanel-99999999-1111-4111-8111-999999999999"
	app.Adapter.Users[testsupport.ProfileTag] = map[string]ports.RemoteUser{
		testsupport.BootstrapID: {StatisticsID: testsupport.BootstrapID, Present: true, Kind: "bootstrap"},
		unknown:                 {StatisticsID: unknown, Present: true, Kind: "managed"},
	}
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile summary = %#v", summary)
	}
	var conflict *domain.ConflictError
	if err := retagProfile(app, profileID, "managed-next"); !errors.As(err, &conflict) {
		t.Fatalf("retag with queued drift removal err = %v, want conflict", err)
	}
	app.Drain()
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 0 {
		t.Fatalf("unknown identity survived: %v", left)
	}
	if err := retagProfile(app, profileID, "managed-next"); err != nil {
		t.Fatalf("retag after drift removal: %v", err)
	}
	var open int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE state IN ('pending','leased','retry_wait')`).Scan(&open)
	if open != 0 {
		t.Fatalf("open drift removals = %d", open)
	}
}

// T144：创建已提交但尚未同步（pending create）的用户同样冻结契约字段；非契约字段（名称、公开地址）随时可改。
func TestProfileNonContractFieldsRemainEditable(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	app.CreateUser("Pending", profileID, nil)
	var conflict *domain.ConflictError
	if err := retagProfile(app, profileID, "managed-next"); !errors.As(err, &conflict) {
		t.Fatalf("retag with pending create err = %v, want conflict", err)
	}
	record, _ := app.Store.Profile(context.Background(), profileID)
	p := record.Profile
	if err := app.Profiles.UpdateProfile(context.Background(), profileID, application.ProfileInput{Name: "Renamed", InboundTag: p.InboundTag,
		PublicHost: "edge.example.com", PublicPort: p.PublicPort, Method: p.Method, Network: p.Network, BootstrapStatisticsID: p.BootstrapStatisticsID,
		ExpectedRevision: p.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatalf("non-contract edit rejected: %v", err)
	}
	record, _ = app.Store.Profile(context.Background(), profileID)
	if record.Profile.Name != "Renamed" || record.Profile.PublicHost != "edge.example.com" || record.Profile.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("profile after non-contract edit = %#v", record.Profile)
	}
}

// T149：未知身份移除永久失败不是安全终结——改 tag 仍被拒绝；Xray 恢复后协调器重新排队并移除，之后才允许改 tag，旧入站无遗留身份。
func TestProfileContractFrozenAfterPermanentDriftRemovalFailureUntilRecovered(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	const unknown = "xpanel-77777777-1111-4111-8111-777777777777"
	app.Adapter.Users[testsupport.ProfileTag] = map[string]ports.RemoteUser{
		testsupport.BootstrapID: {StatisticsID: testsupport.BootstrapID, Present: true, Kind: "bootstrap"},
		unknown:                 {StatisticsID: unknown, Present: true, Kind: "managed"},
	}
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile summary = %#v", summary)
	}
	app.Adapter.Failures["remove_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected, Operation: "remove_user",
		Retryable: false, SafeSummary: "rejected by Xray"}}}
	app.Drain()
	var state string
	_ = app.Store.DB().Read.QueryRow(`SELECT state FROM drift_removals WHERE statistics_id=?`, unknown).Scan(&state)
	if state != string(domain.SyncPermanentFailed) {
		t.Fatalf("drift removal state = %s, want permanent_failed", state)
	}
	var conflict *domain.ConflictError
	if err := retagProfile(app, profileID, "managed-next"); !errors.As(err, &conflict) {
		t.Fatalf("retag after permanent drift failure err = %v, want conflict", err)
	}
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 1 {
		t.Fatalf("identity state on old inbound = %v", left)
	}
	// Xray 恢复：协调器再次发现该身份并重新排队，synchronizer 移除后契约字段才可变更。
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile after recovery = %#v", summary)
	}
	app.Drain()
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 0 {
		t.Fatalf("old inbound still has xpanel- identities: %v", left)
	}
	if err := retagProfile(app, profileID, "managed-next"); err != nil {
		t.Fatalf("retag after recovery: %v", err)
	}
	var failed, succeeded int
	_ = app.Store.DB().Read.QueryRow(`SELECT SUM(state='permanent_failed'),SUM(state='succeeded') FROM drift_removals WHERE statistics_id=?`, unknown).Scan(&failed, &succeeded)
	if failed != 1 || succeeded != 1 {
		t.Fatalf("drift removals failed=%d succeeded=%d", failed, succeeded)
	}
}

// T154：移除永久失败后身份因 Xray 重启/外部移除消失：协调器把失败意图重新排队，synchronizer 确认 absent 并审计，
// 契约字段冻结解除，旧入站无身份，profile 不会被永久锁死。
func TestPermanentDriftRemovalFailureConvergesAfterExternalRemoval(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	const unknown = "xpanel-66666666-1111-4111-8111-666666666666"
	app.Adapter.Users[testsupport.ProfileTag] = map[string]ports.RemoteUser{
		testsupport.BootstrapID: {StatisticsID: testsupport.BootstrapID, Present: true, Kind: "bootstrap"},
		unknown:                 {StatisticsID: unknown, Present: true, Kind: "managed"},
	}
	app.ReconcileOnce()
	app.Adapter.Failures["remove_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected, Operation: "remove_user",
		Retryable: false, SafeSummary: "rejected by Xray"}}}
	app.Drain()
	var conflict *domain.ConflictError
	if err := retagProfile(app, profileID, "managed-next"); !errors.As(err, &conflict) {
		t.Fatalf("retag after permanent failure err = %v, want conflict", err)
	}
	// Xray 重启（或运维外部移除）：身份不再出现在 ListUsers 中。
	app.Adapter.Restart()
	app.Adapter.Users[testsupport.ProfileTag][testsupport.BootstrapID] = ports.RemoteUser{StatisticsID: testsupport.BootstrapID, Present: true, Kind: "bootstrap"}
	summary := app.ReconcileOnce()
	if summary.ConfirmedAbsent != 1 || summary.RemovedUnknown != 0 {
		t.Fatalf("reconcile after external removal = %#v", summary)
	}
	app.Drain()
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 0 {
		t.Fatalf("old inbound still has identities: %v", left)
	}
	var failed, succeeded, open int
	_ = app.Store.DB().Read.QueryRow(`SELECT SUM(state='permanent_failed'),SUM(state='succeeded'),SUM(state IN ('pending','leased','retry_wait')) FROM drift_removals WHERE statistics_id=?`, unknown).Scan(&failed, &succeeded, &open)
	if failed != 1 || succeeded != 1 || open != 0 {
		t.Fatalf("drift removals failed=%d succeeded=%d open=%d", failed, succeeded, open)
	}
	var confirmed int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result='succeeded' AND safe_summary LIKE '%absent%'`, domain.ActionReconcileRemovedUnknown).Scan(&confirmed)
	if confirmed != 1 {
		t.Fatalf("absence confirmation audits = %d, want 1", confirmed)
	}
	if err := retagProfile(app, profileID, "managed-next"); err != nil {
		t.Fatalf("retag after confirmed absence: %v", err)
	}
	stale, err := app.Store.StaleDriftRemovals(context.Background(), profileID)
	if err != nil || len(stale) != 0 {
		t.Fatalf("stale removals after convergence = %d, %v", len(stale), err)
	}
	if summary := app.ReconcileOnce(); summary.ConfirmedAbsent != 0 {
		t.Fatalf("confirmation re-queued after convergence: %#v", summary)
	}
}

// T155：固定时钟下“旧移除成功 → 身份重现 → 同毫秒新移除永久失败”：旧成功不得被误认成后续确认，
// 契约字段在真正的后续可租约确认前一律拒绝；确认后只解冻一次且旧入站无遗留身份。
func TestOldSuccessDoesNotCoverSameMillisecondPermanentFailure(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	const unknown = "xpanel-55555555-1111-4111-8111-555555555555"
	appear := func() {
		app.Adapter.Users[testsupport.ProfileTag] = map[string]ports.RemoteUser{
			testsupport.BootstrapID: {StatisticsID: testsupport.BootstrapID, Present: true, Kind: "bootstrap"},
			unknown:                 {StatisticsID: unknown, Present: true, Kind: "managed"},
		}
	}
	// 旧成功：时钟固定，本测试中所有记录的 created_at 都是同一毫秒。
	appear()
	app.ReconcileOnce()
	app.Drain()
	var succeeded int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE statistics_id=? AND state='succeeded'`, unknown).Scan(&succeeded)
	if succeeded != 1 {
		t.Fatalf("initial removal succeeded = %d", succeeded)
	}
	// 身份重现，新移除在同一毫秒永久失败。
	appear()
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile after reappearance = %#v", summary)
	}
	app.Adapter.Failures["remove_user"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected, Operation: "remove_user",
		Retryable: false, SafeSummary: "rejected by Xray"}}}
	app.Drain()
	var distinctCreated int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(DISTINCT created_at) FROM drift_removals WHERE statistics_id=?`, unknown).Scan(&distinctCreated)
	if distinctCreated != 1 {
		t.Fatalf("test premise broken: created_at values = %d, want all in the same millisecond", distinctCreated)
	}
	var conflict *domain.ConflictError
	for _, attempt := range []func() error{
		func() error { return retagProfile(app, profileID, "managed-next") },
		func() error {
			record, _ := app.Store.Profile(context.Background(), profileID)
			p := record.Profile
			return app.Profiles.UpdateProfile(context.Background(), profileID, application.ProfileInput{Name: p.Name, InboundTag: p.InboundTag, PublicHost: p.PublicHost,
				PublicPort: p.PublicPort, Method: p.Method, Network: p.Network, BootstrapStatisticsID: "bootstrap-other",
				ExpectedRevision: p.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
		},
	} {
		if err := attempt(); !errors.As(err, &conflict) {
			t.Fatalf("contract change accepted while a permanent failure is unsuperseded: %v", err)
		}
	}
	stale, err := app.Store.StaleDriftRemovals(context.Background(), profileID)
	if err != nil || len(stale) != 1 || stale[0].StatisticsID != unknown {
		t.Fatalf("stale removals = %#v, %v", stale, err)
	}
	// 真正的后续可租约确认：身份仍在 → 协调器重新排队（显式取代旧失败）→ synchronizer 移除成功。
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 || summary.ConfirmedAbsent != 0 {
		t.Fatalf("reconcile for confirmation = %#v", summary)
	}
	var supersededBy string
	_ = app.Store.DB().Read.QueryRow(`SELECT COALESCE(superseded_by,'') FROM drift_removals WHERE statistics_id=? AND state='permanent_failed'`, unknown).Scan(&supersededBy)
	if supersededBy == "" {
		t.Fatal("new intent did not explicitly supersede the permanent failure")
	}
	if err := retagProfile(app, profileID, "managed-next"); !errors.As(err, &conflict) {
		t.Fatalf("contract change accepted while the confirming intent is still open: %v", err)
	}
	app.Drain()
	if left := managedOnInbound(app, testsupport.ProfileTag); len(left) != 0 {
		t.Fatalf("old inbound still has identities: %v", left)
	}
	if stale, _ := app.Store.StaleDriftRemovals(context.Background(), profileID); len(stale) != 0 {
		t.Fatalf("stale removals after confirmation = %#v", stale)
	}
	if err := retagProfile(app, profileID, "managed-next"); err != nil {
		t.Fatalf("retag after confirmation: %v", err)
	}
	// 只解冻一次：之后的协调不再重新排队，也没有新的意图记录。
	var total int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE statistics_id=?`, unknown).Scan(&total)
	if summary := app.ReconcileOnce(); summary.ConfirmedAbsent != 0 || summary.RemovedUnknown != 0 {
		t.Fatalf("reconcile after unfreeze = %#v", summary)
	}
	var after int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE statistics_id=?`, unknown).Scan(&after)
	if total != 3 || after != total {
		t.Fatalf("drift removal rows = %d → %d, want 3 (success, failure, confirmation) with no further intents", total, after)
	}
}
