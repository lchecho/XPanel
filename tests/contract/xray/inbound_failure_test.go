package xray_test

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 8：端口被外部进程占用时 CreateInbound 报错；AddInbound 非原子，入站仍可能被注册，
// 因此面板必须读后写确认并补偿移除，否则重试会永久卡在 inbound_already_exists（research.md R-003）。
func TestLiveCreateInboundOnOccupiedPortCompensatesAndConverges(t *testing.T) {
	runtime := startRuntime(t)
	occupied := freePort(t)
	external, err := net.Listen("tcp", net.JoinHostPort(listenAddress, strconv.Itoa(occupied)))
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()

	tag := panelTag("occupied")
	command := ports.CreateInboundCommand{InboundTag: tag, ListenAddress: listenAddress, Port: occupied,
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: security.NewRedactedString(testKey('s')),
		Client: ports.InboundClient{StatisticsID: panelTag("occupied-client"), CredentialVersion: 1,
			UserKey: security.NewRedactedString(testKey('u'))}}
	_, createErr := runtime.client.CreateInbound(context.Background(), command)
	var adapterErr *ports.AdapterError
	if !errors.As(createErr, &adapterErr) || adapterErr.Kind != ports.ErrorPortUnavailable {
		t.Fatalf("create on an occupied port = %v", createErr)
	}
	if adapterErr.Retryable {
		t.Fatal("port_unavailable must not be retryable without compensation")
	}

	// 补偿移除：无论入站是否已被注册，移除后重试都不得撞上 inbound_already_exists。
	if _, err := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: tag}); err != nil {
		if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorInboundNotFound {
			t.Fatalf("compensating removal = %v", err)
		}
	}
	// 换端口后收敛。
	retry := command
	retry.Port = freePort(t)
	if _, err := runtime.client.CreateInbound(context.Background(), retry); err != nil {
		t.Fatalf("retry on a free port failed: %v\n%s", err, runtime.diagnostics())
	}
	if !listening(retry.Port) {
		t.Fatalf("port %d is not listening after the compensated retry", retry.Port)
	}
	if !listening(occupied) {
		t.Fatal("the external listener lost its port")
	}
}

// 端口探测存在 TOCTOU 窗口：外部进程在探测之后、AddInbound 之前抢占端口时，
// Xray 自身的 bind 失败必须仍被映射为可识别的错误类别，且入站可被补偿移除。
func TestLiveCreateInboundBindFailureIsRecoverable(t *testing.T) {
	runtime := startRuntime(t)
	tag, port := panelTag("bindfail"), freePort(t)
	createInbound(t, runtime, tag, port, testKey('u'))
	// 第二条入站复用同一端口：Xray 自身不拒绝重复端口，唯一性由面板保证（research.md R-002）。
	second := ports.CreateInboundCommand{InboundTag: panelTag("bindfail-2"), ListenAddress: listenAddress, Port: port,
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: security.NewRedactedString(testKey('s')),
		Client: ports.InboundClient{StatisticsID: panelTag("bindfail-2-client"), CredentialVersion: 1,
			UserKey: security.NewRedactedString(testKey('v'))}}
	_, err := runtime.client.CreateInbound(context.Background(), second)
	var adapterErr *ports.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorPortUnavailable {
		t.Fatalf("duplicate port create = %v (want port_unavailable from the pre-bind probe)", err)
	}
	// 补偿移除后第一条入站不受影响。
	_, _ = runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: second.InboundTag})
	if !listening(port) {
		t.Fatal("the original inbound stopped listening after compensating the duplicate")
	}
}
