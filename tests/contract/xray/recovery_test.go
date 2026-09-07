package xray_test

import (
	"context"
	"errors"
	"testing"
	"time"

	xrayadapter "xpanel/internal/adapter/xray"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 7：真实重启后 boot epoch 前移、面板在运行期创建的入站全部消失且端口释放；
// 按库中记录的原端口整体重建后端口重新监听。运维自有入站由配置文件恢复，全程不变。
func TestLiveRestartDropsPanelInboundsAndRebuildsOnTheSamePorts(t *testing.T) {
	runtime := startRuntime(t)
	bin := contractBinary(t)
	target := ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	activeTag, activePort := panelTag("active"), freePort(t)
	blockedTag, blockedPort := panelTag("blocked"), freePort(t)
	createInbound(t, runtime, activeTag, activePort, testKey('a'))
	createInbound(t, runtime, blockedTag, blockedPort, testKey('b'))

	before, err := runtime.client.Probe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	runtime.restart(t, bin)
	after, err := runtime.client.Probe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !after.BootEpochKnown || !after.BootEpoch.After(before.BootEpoch) {
		t.Fatalf("boot epoch did not advance: before=%s after=%s", before.BootEpoch, after.BootEpoch)
	}
	inbounds, err := runtime.client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, inbound := range inbounds {
		if inbound.PanelManaged {
			t.Fatalf("panel inbound survived the restart: %#v", inbound)
		}
	}
	if listening(activePort) || listening(blockedPort) {
		t.Fatalf("ports were not released by the restart: active=%v blocked=%v", listening(activePort), listening(blockedPort))
	}
	if !listening(runtime.operatorPort) {
		t.Fatal("operator inbound did not come back with the configuration file")
	}

	// 协调：只重建应当监听的入站，且必须用库中记录的原端口。
	createInbound(t, runtime, activeTag, activePort, testKey('a'))
	if !listening(activePort) {
		t.Fatalf("port %d did not come back after reconciliation", activePort)
	}
	if listening(blockedPort) {
		t.Fatalf("port %d of a user that must stay absent was rebuilt", blockedPort)
	}
}

// 契约门禁 8：极短超时下 CreateInbound 的结果不确定——变更可能已生效。
// 读后写确认后不得重复创建，重放必须得到可识别的 inbound_already_exists 而不是静默产生第二条入站。
func TestLiveUncertainCreateInboundConvergesThroughReadAfterWrite(t *testing.T) {
	runtime := startRuntime(t)
	impatient, err := xrayadapter.New(ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()

	tag, port := panelTag("uncertain"), freePort(t)
	command := ports.CreateInboundCommand{InboundTag: tag, ListenAddress: listenAddress, Port: port,
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: security.NewRedactedString(testKey('s')),
		Client: ports.InboundClient{StatisticsID: panelTag("uncertain-client"), CredentialVersion: 1,
			UserKey: security.NewRedactedString(testKey('u'))}}
	_, uncertainErr := impatient.CreateInbound(context.Background(), command)
	var adapterErr *ports.AdapterError
	if uncertainErr != nil && (!errors.As(uncertainErr, &adapterErr) || adapterErr.Kind != ports.ErrorDeadlineExceeded) {
		t.Fatalf("unexpected error kind for the impatient create: %v", uncertainErr)
	}

	// 读后写：确认入站是否已经注册；未注册才重放。
	present := false
	inbounds, err := runtime.client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, inbound := range inbounds {
		if inbound.InboundTag == tag {
			present = true
		}
	}
	if !present {
		if _, err := runtime.client.CreateInbound(context.Background(), command); err != nil {
			if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorInboundAlreadyExists {
				t.Fatalf("replay after read-after-write: %v\n%s", err, runtime.diagnostics())
			}
		}
	}
	count := 0
	inbounds, err = runtime.client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, inbound := range inbounds {
		if inbound.InboundTag == tag {
			count++
			if inbound.UserCount != 1 {
				t.Fatalf("uncertain create produced %d clients", inbound.UserCount)
			}
		}
	}
	if count != 1 {
		t.Fatalf("uncertain mutation produced %d copies of the inbound", count)
	}
	if !listening(port) {
		t.Fatalf("port %d is not listening after convergence", port)
	}
}
