package xray_test

import (
	"context"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

// 契约门禁 1/4：无效 SS2022 密钥在配置检查阶段即失败；Adapter 在构建期拒绝没有受管客户端的入站
// （空客户端会让入站退化为服务端密钥可直连的单用户模式，research.md R-005）。
func TestKnownSS2022ConfigurationFailuresAndEmptyClientRejection(t *testing.T) {
	bin := contractBinary(t)
	apiAddress, inboundAddress := freeAddress(t), freeAddress(t)
	tests := []struct {
		name    string
		clients []map[string]string
		key     string
	}{
		{name: "invalid service key", clients: []map[string]string{{"email": "bootstrap", "password": testKey('b')}}, key: "invalid"},
		{name: "invalid user key", clients: []map[string]string{{"email": "bootstrap", "password": "invalid"}}, key: testKey('s')},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := configWithInbound(baseConfig(apiAddress, freeAddress(t)), "checked", inboundAddress,
				"2022-blake3-aes-256-gcm", test.key, test.clients)
			if err := runConfigCheck(t, bin, config); err == nil {
				t.Fatal("known-invalid SS2022 configuration was accepted")
			}
		})
	}
	// 构建期负例：没有受管客户端的创建请求不得到达 Xray。
	t.Run("inbound without a managed client is rejected before the RPC", func(t *testing.T) {
		runtime := startRuntime(t)
		command := ports.CreateInboundCommand{InboundTag: panelTag("empty"), ListenAddress: listenAddress, Port: freePort(t),
			Method: security.MethodAES256, Network: domain.NetworkTCPUDP, ServerKey: security.NewRedactedString(testKey('s'))}
		if _, err := runtime.client.CreateInbound(context.Background(), command); err == nil {
			t.Fatal("inbound without a managed client was accepted")
		}
		inbounds, err := runtime.client.ListInbounds(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, inbound := range inbounds {
			if inbound.InboundTag == panelTag("empty") {
				t.Fatal("rejected inbound reached Xray")
			}
		}
	})
}

// 契约门禁 2–3：入站模板校验用一次性探针入站证明实例支持运行时入站管理与多用户身份，探针必被移除。
func TestLiveTemplateValidationUsesADisposableProbe(t *testing.T) {
	runtime := startRuntime(t)
	templateID := testsupport.NewID(t)
	probe := ports.TemplateProbe{TemplateID: templateID, ListenAddress: listenAddress, ProbePort: freePort(t), Method: security.MethodAES256}
	capabilities, err := runtime.client.ValidateTemplate(context.Background(), probe)
	if err != nil || !capabilities.Compatible() {
		t.Fatalf("template capabilities = %#v, %v\n%s", capabilities, err, runtime.diagnostics())
	}
	if listening(probe.ProbePort) {
		t.Fatalf("probe port %d is still listening after validation", probe.ProbePort)
	}
	inbounds, err := runtime.client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, inbound := range inbounds {
		if inbound.PanelManaged {
			t.Fatalf("probe inbound survived validation: %#v", inbound)
		}
	}
	// 不支持的加密方法在不接触节点的前提下判为不兼容。
	unsupported, err := runtime.client.ValidateTemplate(context.Background(),
		ports.TemplateProbe{TemplateID: templateID, ListenAddress: listenAddress, ProbePort: freePort(t), Method: "aes-128-gcm"})
	if err != nil || unsupported.Compatible() || unsupported.MethodSupported {
		t.Fatalf("unsupported method capabilities = %#v, %v", unsupported, err)
	}
}
