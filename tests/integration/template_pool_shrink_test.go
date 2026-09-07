package integration

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

// 缩小端口池不影响既有用户：保存仍然成功，池外分配被如实报告，新用户只能落在新池内（FR-009）。
func TestShrinkingThePoolKeepsExistingAssignmentsUsable(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Primary", 35000, 35009)
	first := app.CreateUser("First", templateID, nil)   // 35000
	second := app.CreateUser("Second", templateID, nil) // 35001
	third := app.CreateUser("Third", templateID, nil)   // 35002
	app.Drain()
	outsidePort := third.Inbound.Inbound.Port

	// 把池缩到只覆盖前两个分配。
	record, err := app.Store.Template(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Templates.UpdateTemplate(context.Background(), templateID, application.TemplateInput{
		Name: record.Template.Name, PublicHost: record.Template.PublicHost, ListenAddress: record.Template.ListenAddress,
		PortPoolStart: 35000, PortPoolEnd: 35001, Method: security.MethodAES256, Network: domain.NetworkTCPUDP,
		ExpectedRevision: record.Template.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatalf("shrinking the pool was rejected: %v", err)
	}

	// 既有用户完全不受影响：端口继续监听、计量继续。
	for _, existing := range []domain.ID{first.User.ID, second.User.ID, third.User.ID} {
		current := app.User(existing)
		if !app.Listening(current.Inbound.Inbound.Port) || current.Allocation.PendingSync() {
			t.Fatalf("shrinking the pool disturbed %s: %#v", current.User.DisplayName, current.Allocation)
		}
	}
	usage, err := app.Templates.PortUsage(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	// Assigned 只计池内占用，池外分配单列，两者相加才是该模板下的全部端口。
	if usage.Capacity != 2 || usage.Assigned != 2 || usage.Remaining != 0 || len(usage.Outside) != 1 || usage.Outside[0] != outsidePort {
		t.Fatalf("port usage after shrink = %#v", usage)
	}

	// 新用户只能落在新池内：池已被前两个分配占满，因此自动分配报耗尽。
	_, _, err = app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fourth",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Message != domain.ErrPortPoolExhausted.Message {
		t.Fatalf("creation after shrink = %T %v", err, err)
	}
	// 指定池外端口（哪怕它当前空闲）被拒绝为字段级错误。
	free := 35005
	_, _, err = app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Fourth",
		TemplateID: templateID, Port: &free, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) || invalid.Field != "port" {
		t.Fatalf("out-of-pool creation after shrink = %T %v", err, err)
	}

	// 详情页把池外端口标识出来。
	app.Login()
	_, body := app.Get("/templates/" + templateID.String())
	if !strings.Contains(body, "位于当前端口池之外") || !strings.Contains(body, strconv.Itoa(outsidePort)) {
		t.Fatalf("template detail does not flag the out-of-pool assignment: %s", body)
	}
}
