package integration

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

// SC-012：对一个用户执行创建、禁用、启用、轮换、配额封禁与删除的全过程中，
// 另一个用户的端口可连接性与流量计量必须零偏差。
func TestOneUsersLifecycleNeverDisturbsAnother(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	subject := app.CreateUser("Subject", templateID, &limit)
	bystander := app.CreateUser("Bystander", templateID, nil)
	app.Drain()

	subjectPort := subject.Inbound.Inbound.Port
	bystanderPort := bystander.Inbound.Inbound.Port
	bystanderTag := bystander.Inbound.Inbound.InboundTag
	if subjectPort == bystanderPort {
		t.Fatalf("both users share port %d", subjectPort)
	}
	// 旁观者持续产生流量：每一步之后它的累计计量都必须精确等于已确认的绝对计数。
	// 用生命周期累计值而不是周期内累计值，因为跨配额周期边界时周期计数本就会归零。
	var counted uint64
	advanceTraffic := func(step string) {
		counted += 4096
		app.SetTraffic(bystander, counted, counted)
		app.Collect()
		var uplink, downlink int64
		if err := app.Store.DB().Read.QueryRow(`SELECT uplink_bytes,downlink_bytes FROM allocation_traffic_totals WHERE allocation_id=?`,
			bystander.Allocation.ID.String()).Scan(&uplink, &downlink); err != nil {
			t.Fatal(err)
		}
		if uint64(uplink) != counted || uint64(downlink) != counted {
			t.Fatalf("%s skewed the bystander accounting: up=%d down=%d want %d", step, uplink, downlink, counted)
		}
	}
	assertUndisturbed := func(step string) {
		t.Helper()
		if !app.Listening(bystanderPort) {
			t.Fatalf("%s stopped the bystander port %d", step, bystanderPort)
		}
		current := app.User(bystander.User.ID)
		if current.Inbound.Inbound.Port != bystanderPort || current.Inbound.Inbound.InboundTag != bystanderTag {
			t.Fatalf("%s changed the bystander inbound: %#v", step, current.Inbound.Inbound)
		}
		if current.Allocation.PendingSync() || current.Allocation.ProjectionState != domain.ProjectionPresent {
			t.Fatalf("%s left the bystander allocation unsettled: %#v", step, current.Allocation)
		}
		if _, ok := app.Adapter.Users[bystanderTag][bystander.Identity.StatisticsID]; !ok {
			t.Fatalf("%s removed the bystander client", step)
		}
		advanceTraffic(step)
	}
	assertUndisturbed("baseline")

	revision := func() domain.Revision { return app.User(subject.User.ID).User.Revision }
	steps := []struct {
		name string
		run  func() error
	}{
		{name: "disable", run: func() error {
			_, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: subject.User.ID, Enabled: false,
				ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
		{name: "enable", run: func() error {
			_, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: subject.User.ID, Enabled: true,
				ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
		{name: "rotate", run: func() error {
			_, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: subject.User.ID,
				ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			return err
		}},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		app.Drain()
		assertUndisturbed(step.name)
	}

	// 配额封禁：受测用户端口停止监听，旁观者不受影响。
	app.SetTraffic(subject, uint64(limit), 0)
	app.Collect()
	app.Drain()
	if app.Listening(subjectPort) {
		t.Fatalf("subject port %d kept listening after the quota block", subjectPort)
	}
	assertUndisturbed("quota_block")

	// 新周期恢复后再删除：受测端口先回来再彻底释放，旁观者始终稳定。
	app.Clock.Set(app.User(subject.User.ID).Cycle.EndsAt.Add(time.Second))
	app.Rollover()
	app.Drain()
	if !app.Listening(subjectPort) {
		t.Fatalf("subject port %d did not come back after the cycle boundary", subjectPort)
	}
	assertUndisturbed("quota_restore")

	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: subject.User.ID,
		ExpectedRevision: revision(), RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	if app.Listening(subjectPort) {
		t.Fatalf("subject port %d was not released by the deletion", subjectPort)
	}
	assertUndisturbed("delete")

	// 释放的端口可以再次分配，新用户不影响旁观者，也不复用旧身份。
	reused, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Successor",
		TemplateID: templateID, Port: &subjectPort, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if err != nil {
		t.Fatalf("reassigning the released port: %v", err)
	}
	app.Drain()
	successor := app.User(reused)
	if successor.Inbound.Inbound.Port != subjectPort || !app.Listening(subjectPort) {
		t.Fatalf("successor port = %d want %d listening", successor.Inbound.Inbound.Port, subjectPort)
	}
	if successor.Identity.StatisticsID == subject.Identity.StatisticsID ||
		successor.Inbound.Inbound.InboundTag == subject.Inbound.Inbound.InboundTag {
		t.Fatal("the reassigned port reused the previous identity or inbound tag")
	}
	assertUndisturbed("port_reuse")
	var records []ports.UserRecord = app.ListUsers()
	if len(records) != 2 {
		t.Fatalf("live users = %d, want 2", len(records))
	}
}
