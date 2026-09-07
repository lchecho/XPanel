package xray_test

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 真实故障契约：轮换只能在既有入站内先删后加同一 email——Xray 不接受同 email 的重复添加，
// 也不允许原地替换密钥。因此「空窗」不可避免，面板必须把它压在单个租约步骤内并有补偿动作（FR-019）。
func TestLiveRotationSemanticsWithinOneInbound(t *testing.T) {
	runtime := startRuntime(t)
	tag, port := panelTag("rotation"), freePort(t)
	createInbound(t, runtime, tag, port, testKey('u'))
	inbound := ports.RuntimeInbound{InboundTag: tag, Method: security.MethodAES256}
	statisticsID := panelTag("rotation-client")

	// 1) 同一 email 的重复添加被拒绝：无法原地替换密钥，必须先删后加。
	replace := ports.AddUserCommand{InboundTag: tag, StatisticsID: statisticsID, CredentialVersion: 2,
		UserKey: security.NewRedactedString(testKey('v'))}
	_, err := runtime.client.AddUser(context.Background(), replace)
	var adapterErr *ports.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorUserAlreadyExists {
		t.Fatalf("in-place key replacement = %v (want user_already_exists)", err)
	}

	// 2) 先删后加：端口全程保持监听，入站与标签不变。
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
		StatisticsID: statisticsID}); err != nil {
		t.Fatal(err)
	}
	if !listening(port) {
		t.Fatalf("port %d stopped listening while the inbound was empty", port)
	}
	empty, err := runtime.client.ListUsers(context.Background(), inbound)
	if err != nil || len(empty) != 0 {
		t.Fatalf("users after removal = %#v, %v", empty, err)
	}
	if _, err := runtime.client.AddUser(context.Background(), replace); err != nil {
		t.Fatalf("re-adding the rotated credential failed: %v\n%s", err, runtime.diagnostics())
	}
	users, err := runtime.client.ListUsers(context.Background(), inbound)
	if err != nil || len(users) != 1 || users[0].StatisticsID != statisticsID {
		t.Fatalf("users after rotation = %#v, %v", users, err)
	}
	if !listening(port) {
		t.Fatalf("port %d is not listening after rotation", port)
	}

	// 3) 补偿路径：加不回去时移除整条入站是可行的，且端口随之释放，重建可用同一端口。
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
		StatisticsID: statisticsID}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: tag}); err != nil {
		t.Fatal(err)
	}
	if listening(port) {
		t.Fatalf("port %d was not released by the compensating removal", port)
	}
	createInbound(t, runtime, tag, port, testKey('w'))
	if !listening(port) {
		t.Fatalf("port %d did not come back after the rebuild", port)
	}
}
