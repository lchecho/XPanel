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
