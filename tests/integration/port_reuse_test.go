package integration

import (
	"context"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/testsupport"
)

// 端口回收后再分配给新用户时，新用户 MUST 拿到全新的入站标签、服务端密钥、用户密钥与统计标识，
// 旧连接信息在该端口上不再可用（spec §用户故事 3 场景 5）。
func TestReassignedPortCarriesEntirelyNewCredentials(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	original := app.CreateUser("Original", templateID, nil)
	app.Drain()
	port := original.Inbound.Inbound.Port
	before, err := app.Connections.BuildConnectionInfo(context.Background(), original.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldServerKey := app.Adapter.Inbounds[original.Inbound.Inbound.InboundTag].ServerKey

	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: original.User.ID,
		ExpectedRevision: app.User(original.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()

	successorID, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Successor",
		TemplateID: templateID, Port: &port, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if err != nil {
		t.Fatal(err)
	}
	app.Drain()
	successor := app.User(successorID)
	if successor.Inbound.Inbound.Port != port {
		t.Fatalf("successor port = %d want %d", successor.Inbound.Inbound.Port, port)
	}
	if successor.Inbound.Inbound.InboundTag == original.Inbound.Inbound.InboundTag {
		t.Fatal("the successor reused the previous inbound tag")
	}
	if successor.Identity.StatisticsID == original.Identity.StatisticsID {
		t.Fatal("the successor reused the previous statistics identity")
	}
	if newServerKey := app.Adapter.Inbounds[successor.Inbound.Inbound.InboundTag].ServerKey; newServerKey == oldServerKey {
		t.Fatal("the successor reused the previous server key")
	}
	after, err := app.Connections.BuildConnectionInfo(context.Background(), successorID)
	if err != nil {
		t.Fatal(err)
	}
	if after.URI.Reveal() == before.URI.Reveal() || after.Password.Reveal() == before.Password.Reveal() {
		t.Fatal("the successor received the previous connection information")
	}
	if after.Port != port {
		t.Fatalf("successor connection port = %d want %d", after.Port, port)
	}
	// 旧用户的凭证已销毁：连接信息不再可取。
	if _, err := app.Connections.BuildConnectionInfo(context.Background(), original.User.ID); err == nil {
		t.Fatal("the deleted user still exposes connection information")
	}
	// 该端口上现在只有新用户的受管客户端。
	if _, stale := app.Adapter.Users[successor.Inbound.Inbound.InboundTag][original.Identity.StatisticsID]; stale {
		t.Fatal("the previous identity is still present on the reassigned port")
	}
}
