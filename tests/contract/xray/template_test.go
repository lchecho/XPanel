package xray_test

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/application"
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
		{name: "invalid service key", clients: []map[string]string{{"email": "operator", "password": testKey('b')}}, key: "invalid"},
		{name: "invalid user key", clients: []map[string]string{{"email": "operator", "password": "invalid"}}, key: testKey('s')},
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
	probe := ports.TemplateProbe{TemplateID: templateID, ListenAddress: listenAddress, ProbePort: freePort(t),
		Method: security.MethodAES256, Network: domain.NetworkTCPUDP}
	capabilities, err := runtime.client.ValidateTemplate(context.Background(), probe)
	if err != nil || !capabilities.Compatible() {
		t.Fatalf("template capabilities = %#v, %v\n%s", capabilities, err, runtime.diagnostics())
	}
	// 四项能力都必须被证实：能建、能拆、多用户身份、用户级统计可读（FR-005）。
	if !capabilities.InboundCreatable || !capabilities.InboundRemovable || !capabilities.MultiUserSupported ||
		!capabilities.TrafficAccounted {
		t.Fatalf("capabilities did not prove the full contract: %#v", capabilities)
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
		ports.TemplateProbe{TemplateID: templateID, ListenAddress: listenAddress, ProbePort: freePort(t),
			Method: "aes-128-gcm", Network: domain.NetworkTCPUDP})
	if err != nil || unsupported.Compatible() || unsupported.MethodSupported {
		t.Fatalf("unsupported method capabilities = %#v, %v", unsupported, err)
	}
}

// 契约门禁 5（前置）：漏配 policy.levels."0".statsUserUplink/statsUserDownlink 的节点
// MUST 在创建任何用户之前就被判为不兼容——否则所有用户的用量恒为零，配额形同虚设（FR-005）。
func TestLiveTemplateValidationRejectsNodesWithoutUserTrafficStats(t *testing.T) {
	apiAddress, operatorAddress := freeAddress(t), freeAddress(t)
	config := baseConfig(apiAddress, operatorAddress)
	delete(config, "policy") // 保留 stats 与 API，只去掉用户级统计策略
	runtime, err := launchRuntime(t, apiAddress, config)
	if err != nil {
		t.Fatal(err)
	}
	runtime.operator, runtime.operatorPort = operatorAddress, portOf(t, operatorAddress)

	probe := ports.TemplateProbe{TemplateID: testsupport.NewID(t), ListenAddress: listenAddress,
		ProbePort: freePort(t), Method: security.MethodAES256, Network: domain.NetworkTCPUDP}
	capabilities, err := runtime.client.ValidateTemplate(context.Background(), probe)
	if err != nil {
		t.Fatalf("validation against a node without the policy failed outright: %v\n%s", err, runtime.diagnostics())
	}
	// 入站生命周期与多用户身份都没问题，唯独统计不可读。
	if !capabilities.InboundCreatable || !capabilities.InboundRemovable || !capabilities.MultiUserSupported {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if capabilities.TrafficAccounted || capabilities.Compatible() {
		t.Fatalf("a node without user traffic stats was reported compatible: %#v", capabilities)
	}
	if capabilities.CompatibilityReason == "" {
		t.Fatal("no reason was reported for the missing traffic statistics")
	}
	// 探针照常被清理，不留下任何面板入站。
	if listening(probe.ProbePort) {
		t.Fatalf("probe port %d is still listening", probe.ProbePort)
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
}

// T088：能力门禁必须按模板实际的加密方式与网络能力做探测，而不是硬编码一种组合。
// 每个组合都跑「有 policy 通过 / 无 policy 判不兼容 / 重复验证仍然通过」三种情形。
func TestLiveTemplateValidationMatrixAcrossMethodsAndNetworks(t *testing.T) {
	for _, method := range []string{security.MethodAES128, security.MethodAES256} {
		for _, network := range []domain.Network{domain.NetworkTCP, domain.NetworkUDP, domain.NetworkTCPUDP} {
			t.Run(method+"/"+string(network), func(t *testing.T) {
				runtime := startRuntime(t)
				probe := func() ports.TemplateProbe {
					return ports.TemplateProbe{TemplateID: testsupport.NewID(t), ListenAddress: listenAddress,
						ProbePort: freePort(t), Method: method, Network: network}
				}
				capabilities, err := runtime.client.ValidateTemplate(context.Background(), probe())
				if err != nil || !capabilities.Compatible() {
					t.Fatalf("capabilities = %#v, %v\n%s", capabilities, err, runtime.diagnostics())
				}
				// 重复验证必须同样通过：探针身份每次都是新的，不会被上一次的残留计数影响，
				// 也不会因为残留计数而在缺少 policy 时误判为通过。
				repeat, err := runtime.client.ValidateTemplate(context.Background(), probe())
				if err != nil || !repeat.Compatible() {
					t.Fatalf("repeat validation = %#v, %v", repeat, err)
				}
				// 探针入站与端口都不残留。
				inbounds, err := runtime.client.ListInbounds(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, inbound := range inbounds {
					if inbound.PanelManaged {
						t.Fatalf("probe inbound survived: %#v", inbound)
					}
				}
			})
		}
	}
}

// T088：缺少 policy 时，每种方法与网络组合都必须判为不兼容——不能因为某个组合没发流量而误通过。
func TestLiveTemplateValidationMatrixRejectsMissingPolicy(t *testing.T) {
	for _, network := range []domain.Network{domain.NetworkTCP, domain.NetworkUDP, domain.NetworkTCPUDP} {
		t.Run(string(network), func(t *testing.T) {
			apiAddress, operatorAddress := freeAddress(t), freeAddress(t)
			config := baseConfig(apiAddress, operatorAddress)
			delete(config, "policy")
			runtime, err := launchRuntime(t, apiAddress, config)
			if err != nil {
				t.Fatal(err)
			}
			runtime.operator, runtime.operatorPort = operatorAddress, portOf(t, operatorAddress)
			capabilities, err := runtime.client.ValidateTemplate(context.Background(), ports.TemplateProbe{
				TemplateID: testsupport.NewID(t), ListenAddress: listenAddress, ProbePort: freePort(t),
				Method: security.MethodAES256, Network: network})
			if err != nil {
				t.Fatalf("validation failed outright: %v\n%s", err, runtime.diagnostics())
			}
			if capabilities.TrafficAccounted || capabilities.Compatible() {
				t.Fatalf("%s without policy reported compatible: %#v", network, capabilities)
			}
		})
	}
}

// T089 契约：兼容结论绑定当前 Xray 启动纪元。在完整 policy 下验证通过后，
// 用同一管理端点重启到缺少 policy 的配置，旧的 compatible 缓存必须立即失效、新建用户被拒绝；
// 恢复 policy 并重新验证后才允许创建。
func TestLiveCapabilityEvidenceExpiresWhenTheNodeRestartsWithoutStatsPolicy(t *testing.T) {
	apiAddress, operatorAddress := freeAddress(t), freeAddress(t)
	full := baseConfig(apiAddress, operatorAddress)
	runtime, err := launchRuntime(t, apiAddress, full)
	if err != nil {
		t.Fatal(err)
	}
	runtime.operator, runtime.operatorPort = operatorAddress, portOf(t, operatorAddress)
	target := ports.InstanceTarget{APIEndpoint: runtime.api, ExpectedVersion: wantRuntime, RPCTimeout: 500 * time.Millisecond}
	app := testsupport.NewWith(t, testsupport.Options{Adapter: runtime.client, Target: &target})
	app.Clock.Set(time.Now().UTC())

	// 完整 policy 下：门禁通过，可以创建用户。
	templateID := registerLiveTemplate(t, app, "Primary", 6)
	first := app.CreateUser("First", templateID, nil)
	convergeAll(t, app, []ports.UserRecord{first})

	// 重启到缺少 policy 的配置：管理端点不变，但节点能力变了。
	// 先等一秒多，让重启前后的 boot epoch 差值明确超过量化容差（uptime 是整秒）。
	time.Sleep(1100 * time.Millisecond)
	degraded := baseConfig(apiAddress, operatorAddress)
	delete(degraded, "policy")
	runtime.restartWith(t, contractBinary(t), degraded)
	app.Clock.Set(app.Clock.Now().Add(time.Minute))

	summary := app.ReconcileOnce()
	if summary.Revalidated != 1 {
		t.Fatalf("reconcile summary = %#v, want the stale evidence invalidated", summary)
	}
	record, err := app.Store.Template(context.Background(), templateID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Template.Compatibility == domain.CompatibilityCompatible {
		t.Fatalf("the template stayed compatible on a node that lost its stats policy: %#v", record.Template)
	}
	if _, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Rejected",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err == nil {
		t.Fatal("a user was created against a node that no longer reports user traffic")
	}

	// 恢复 policy 并重新验证后才允许创建。
	time.Sleep(1100 * time.Millisecond)
	runtime.restartWith(t, contractBinary(t), full)
	app.Clock.Set(app.Clock.Now().Add(time.Minute))
	app.ReconcileOnce()
	if err := app.Validator.ValidateNow(context.Background(), templateID); err != nil {
		t.Fatal(err)
	}
	restored, _ := app.Store.Template(context.Background(), templateID)
	instance, _ := app.Store.ManagedInstance(context.Background())
	if restored.Template.Compatibility != domain.CompatibilityCompatible ||
		restored.Template.ValidatedGeneration != instance.CapabilityGeneration {
		t.Fatalf("template after restoring the policy = %#v (instance generation %d)", restored.Template,
			instance.CapabilityGeneration)
	}
	if _, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Allowed",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatalf("creation after restoring the policy: %v", err)
	}
}
