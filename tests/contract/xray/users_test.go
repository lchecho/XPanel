package xray_test

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 3：在面板运行期创建的专属入站上，增删客户端的语义稳定——重复添加与移除缺失都有可识别的错误类别。
func TestLiveUserMutationContractOnADedicatedInbound(t *testing.T) {
	runtime := startRuntime(t)
	tag, port := panelTag("users"), freePort(t)
	createInbound(t, runtime, tag, port, testKey('u'))
	inbound := ports.RuntimeInbound{InboundTag: tag, Method: security.MethodAES256}

	users, err := runtime.client.ListUsers(context.Background(), inbound)
	if err != nil || len(users) != 1 || users[0].Kind != "managed" {
		t.Fatalf("initial users = %#v, %v", users, err)
	}
	command := ports.AddUserCommand{InboundTag: tag, StatisticsID: panelTag("550e8400-e29b-41d4-a716-446655440000"),
		CredentialVersion: 1, UserKey: security.NewRedactedString(testKey('m'))}
	if _, err := runtime.client.AddUser(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	var adapterErr *ports.AdapterError
	if _, err := runtime.client.AddUser(context.Background(), command); !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorUserAlreadyExists {
		t.Fatalf("duplicate add = %v", err)
	}
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag, StatisticsID: command.StatisticsID}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag, StatisticsID: command.StatisticsID}); !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorUserNotFound {
		t.Fatalf("missing-user removal = %v", err)
	}
	// 命名空间之外的入站不接受面板的客户端变更。
	if _, err := runtime.client.AddUser(context.Background(), ports.AddUserCommand{InboundTag: operatorInboundTag,
		StatisticsID: panelTag("intruder"), CredentialVersion: 1, UserKey: security.NewRedactedString(testKey('z'))}); err == nil {
		t.Fatal("panel added a client to an inbound outside its namespace")
	}
}
