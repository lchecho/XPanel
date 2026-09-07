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

// changeListenAddress 尝试修改模板的监听地址，这是新模型下的契约字段之一（另一个是加密方式）。
func changeListenAddress(app *testsupport.App, id domain.ID, listen string) error {
	record, err := app.Store.Template(context.Background(), id)
	if err != nil {
		return err
	}
	template := record.Template
	return app.Templates.UpdateTemplate(context.Background(), id, application.TemplateInput{Name: template.Name,
		PublicHost: template.PublicHost, ListenAddress: listen, PortPoolStart: template.Pool.Start,
		PortPoolEnd: template.Pool.End, Method: template.Method, Network: template.Network,
		ExpectedRevision: template.Revision, RequestID: testsupport.NewID(app.T), ActorID: app.AdminID})
}

// panelInboundTags 返回 fake 中全部面板命名空间内的入站标签。
func panelInboundTags(app *testsupport.App) []string {
	var tags []string
	for _, tag := range app.Adapter.InboundTags() {
		if strings.HasPrefix(tag, domain.NamespacePrefix) {
			tags = append(tags, tag)
		}
	}
	return tags
}

// 删除事务已提交但入站移除尚未确认时不得修改契约字段；确认 absent 后才允许，旧监听地址上不遗留面板入站。
func TestContractFrozenUntilDeletedUserInboundRemoved(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	user := app.CreateUser("Leaving", templateID, nil)
	app.Drain()
	if len(panelInboundTags(app)) != 1 {
		t.Fatalf("inbound not created: %v", panelInboundTags(app))
	}
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: user.User.ID,
		ExpectedRevision: app.User(user.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	var conflict *domain.ConflictError
	if err := changeListenAddress(app, templateID, "0.0.0.0"); !errors.As(err, &conflict) {
		t.Fatalf("listen address change with unconfirmed removal err = %v, want conflict", err)
	}
	if left := panelInboundTags(app); len(left) != 1 {
		t.Fatalf("inbound vanished without the synchronizer: %v", left)
	}
	record, _ := app.Store.Template(context.Background(), templateID)
	if record.Template.ListenAddress != testsupport.DefaultListenAddress {
		t.Fatalf("listen address changed despite conflict: %s", record.Template.ListenAddress)
	}
	app.Drain()
	if left := panelInboundTags(app); len(left) != 0 {
		t.Fatalf("panel inbound still present after removal: %v", left)
	}
	if err := changeListenAddress(app, templateID, "0.0.0.0"); err != nil {
		t.Fatalf("listen address change after confirmed absence: %v", err)
	}
	record, _ = app.Store.Template(context.Background(), templateID)
	if record.Template.ListenAddress != "0.0.0.0" || record.Template.Compatibility != domain.CompatibilityUnverified {
		t.Fatalf("template after contract change = %#v", record.Template)
	}
}

// 命名空间内的孤立入站移除意图已排队时不得修改契约字段；synchronizer 执行完毕后才允许。
func TestContractFrozenWhileOrphanInboundRemovalQueued(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	const orphan = "xpanel-99999999-1111-4111-8111-999999999999"
	app.Adapter.AddOrphanInbound(orphan, 30500)
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile summary = %#v", summary)
	}
	var conflict *domain.ConflictError
	if err := changeListenAddress(app, templateID, "0.0.0.0"); !errors.As(err, &conflict) {
		t.Fatalf("contract change with queued drift removal err = %v, want conflict", err)
	}
	app.Drain()
	if left := panelInboundTags(app); len(left) != 0 {
		t.Fatalf("orphan inbound survived: %v", left)
	}
	if err := changeListenAddress(app, templateID, "0.0.0.0"); err != nil {
		t.Fatalf("contract change after drift removal: %v", err)
	}
	var open int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM drift_removals WHERE state IN ('pending','leased','retry_wait')`).Scan(&open)
	if open != 0 {
		t.Fatalf("open drift removals = %d", open)
	}
}

// 待同步的创建同样冻结契约字段；名称、公开地址与端口池随时可改。
func TestNonContractFieldsRemainEditable(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	app.CreateUser("Pending", templateID, nil)
	var conflict *domain.ConflictError
	if err := changeListenAddress(app, templateID, "0.0.0.0"); !errors.As(err, &conflict) {
		t.Fatalf("contract change with pending create err = %v, want conflict", err)
	}
	record, _ := app.Store.Template(context.Background(), templateID)
	template := record.Template
	if err := app.Templates.UpdateTemplate(context.Background(), templateID, application.TemplateInput{Name: "Renamed",
		PublicHost: "edge.example.com", ListenAddress: template.ListenAddress, PortPoolStart: template.Pool.Start,
		PortPoolEnd: template.Pool.End + 10, Method: template.Method, Network: template.Network,
		ExpectedRevision: template.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatalf("non-contract edit rejected: %v", err)
	}
	record, _ = app.Store.Template(context.Background(), templateID)
	if record.Template.Name != "Renamed" || record.Template.PublicHost != "edge.example.com" ||
		record.Template.Pool.End != template.Pool.End+10 || record.Template.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("template after non-contract edit = %#v", record.Template)
	}
}

// T149：孤立入站移除永久失败不是安全终结——契约字段仍冻结；Xray 恢复后重新排队并移除，之后才解冻。
func TestContractFrozenAfterPermanentDriftFailureUntilRecovered(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	const orphan = "xpanel-77777777-1111-4111-8111-777777777777"
	app.Adapter.AddOrphanInbound(orphan, 30501)
	app.ReconcileOnce()
	app.Adapter.Failures["remove_inbound"] = []xrayfake.Failure{{Err: &ports.AdapterError{Kind: ports.ErrorUpstreamRejected,
		Operation: "remove_inbound", Retryable: false, SafeSummary: "rejected by Xray"}}}
	app.Drain()
	var state string
	_ = app.Store.DB().Read.QueryRow(`SELECT state FROM drift_removals WHERE inbound_tag=?`, orphan).Scan(&state)
	if state != string(domain.SyncPermanentFailed) {
		t.Fatalf("drift removal state = %s, want permanent_failed", state)
	}
	var conflict *domain.ConflictError
	if err := changeListenAddress(app, templateID, "0.0.0.0"); !errors.As(err, &conflict) {
		t.Fatalf("contract change after permanent drift failure err = %v, want conflict", err)
	}
	// Xray 恢复：协调器重新排队，synchronizer 移除成功后契约字段才可变更。
	if summary := app.ReconcileOnce(); summary.RemovedUnknown != 1 {
		t.Fatalf("reconcile after recovery = %#v", summary)
	}
	app.Drain()
	if left := panelInboundTags(app); len(left) != 0 {
		t.Fatalf("orphan inbound still present: %v", left)
	}
	if err := changeListenAddress(app, templateID, "0.0.0.0"); err != nil {
		t.Fatalf("contract change after recovery: %v", err)
	}
	var failed, succeeded int
	_ = app.Store.DB().Read.QueryRow(`SELECT SUM(state='permanent_failed'),SUM(state='succeeded') FROM drift_removals WHERE inbound_tag=?`, orphan).Scan(&failed, &succeeded)
	if failed != 1 || succeeded != 1 {
		t.Fatalf("drift removals failed=%d succeeded=%d", failed, succeeded)
	}
}
