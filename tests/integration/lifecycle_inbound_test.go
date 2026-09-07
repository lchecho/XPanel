package integration

import (
	"context"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// 生命周期的每一步都在端口层面可验证：禁用/启用复用原端口，轮换期间端口持续监听，删除后端口回池。
func TestLifecycleKeepsThePortStableUntilDeletion(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Subject", templateID, nil)
	app.Drain()
	port, tag := record.Inbound.Inbound.Port, record.Inbound.Inbound.InboundTag
	if !app.Listening(port) {
		t.Fatalf("port %d is not listening after creation", port)
	}

	revision := func() domain.Revision { return app.User(record.User.ID).User.Revision }
	// 禁用：整条入站被移除，端口停止监听，但端口分配保留给该用户。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: false,
		ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	if app.Listening(port) {
		t.Fatalf("port %d kept listening after disable", port)
	}
	if _, present := app.Adapter.Inbounds[tag]; present {
		t.Fatal("the inbound survived disable without a managed client")
	}
	disabled := app.User(record.User.ID)
	if disabled.Inbound.Inbound.Port != port || disabled.Inbound.Inbound.ReleasedAt != nil {
		t.Fatalf("disable released the port assignment: %#v", disabled.Inbound.Inbound)
	}

	// 启用：以同一端口和同一入站标签重建。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: true,
		ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	enabled := app.User(record.User.ID)
	if enabled.Inbound.Inbound.Port != port || enabled.Inbound.Inbound.InboundTag != tag || !app.Listening(port) {
		t.Fatalf("enable did not reuse the port and tag: %#v", enabled.Inbound.Inbound)
	}

	// 轮换：端口与入站不变，监听不中断，只有密钥版本前进。
	callsBefore := adapterCalls(app, "remove_inbound") + adapterCalls(app, "create_inbound")
	if _, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: record.User.ID,
		ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	rotated := app.User(record.User.ID)
	if rotated.Inbound.Inbound.Port != port || rotated.Inbound.Inbound.InboundTag != tag || !app.Listening(port) {
		t.Fatalf("rotation disturbed the inbound: %#v", rotated.Inbound.Inbound)
	}
	if after := adapterCalls(app, "remove_inbound") + adapterCalls(app, "create_inbound"); after != callsBefore {
		t.Fatalf("rotation touched the inbound lifecycle: %d extra calls", after-callsBefore)
	}
	remote := app.Adapter.Users[tag][rotated.Identity.StatisticsID]
	if remote.CredentialVersion != rotated.Allocation.DesiredCredentialVersion {
		t.Fatalf("rotated credential version = %d want %d", remote.CredentialVersion, rotated.Allocation.DesiredCredentialVersion)
	}

	// 删除：入站移除确认后端口回池。
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: record.User.ID,
		ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	if app.Listening(port) {
		t.Fatalf("port %d kept listening after deletion", port)
	}
	deleted := app.User(record.User.ID)
	if deleted.Inbound.Inbound.ReleasedAt == nil {
		t.Fatal("deletion did not release the port assignment")
	}
	assigned, err := app.Store.AssignedPorts(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	for _, used := range assigned {
		if used == port {
			t.Fatalf("port %d is still counted as assigned", port)
		}
	}
}

func adapterCalls(app *testsupport.App, operation string) int {
	count := 0
	for _, call := range app.Adapter.Calls {
		if call.Operation == operation {
			count++
		}
	}
	return count
}
