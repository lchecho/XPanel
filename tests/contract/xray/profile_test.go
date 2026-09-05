package xray_test

import (
	"context"
	"testing"

	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 3：无效密钥在配置检查阶段即失败；空 clients 被 Xray 26.3.27 接受为单用户模式，
// 因此必须在运行期由 ValidateProfile 判为不兼容（无法 AddUser 的单用户 inbound 不得成为目标）。
func TestKnownSS2022ConfigurationFailures(t *testing.T) {
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
			config := runtimeConfig(apiAddress, inboundAddress, "2022-blake3-aes-256-gcm", test.key, test.clients)
			if err := runConfigCheck(t, bin, config); err == nil {
				t.Fatal("known-invalid SS2022 configuration was accepted")
			}
		})
	}
	t.Run("empty clients enters unsupported single-user mode", func(t *testing.T) {
		runtime := startCustomRuntime(t, []map[string]string{})
		capabilities, err := runtime.client.ValidateProfile(context.Background(),
			ports.RuntimeProfile{InboundTag: "managed", Method: security.MethodAES256, BootstrapStatisticsID: "bootstrap"})
		if err != nil {
			t.Fatal(err)
		}
		if capabilities.Compatible() || capabilities.MultiUserSupported {
			t.Fatalf("single-user SS2022 inbound reported as compatible: %#v", capabilities)
		}
	})
}
