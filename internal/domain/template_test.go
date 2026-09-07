package domain

import (
	"errors"
	"testing"
	"time"

	"xpanel/internal/security"
)

func TestInboundTemplateValidation(t *testing.T) {
	id, _ := NewID()
	instance, _ := NewID()
	now := time.Now()
	template, err := NewInboundTemplate(id, instance, " Main ", "example.com", "0.0.0.0", 20000, 20099,
		security.MethodAES256, NetworkTCPUDP, now)
	if err != nil {
		t.Fatal(err)
	}
	if template.NormalizedName != "main" || template.Compatibility != CompatibilityUnverified {
		t.Fatalf("template = %#v", template)
	}
	if template.Pool.Capacity() != 100 || template.ListenAddress != "0.0.0.0" {
		t.Fatalf("pool = %#v listen = %q", template.Pool, template.ListenAddress)
	}

	tests := []struct {
		name       string
		host       string
		listen     string
		start, end int
		method     string
		network    Network
		field      string
	}{
		{name: "空主机名", host: "", listen: "0.0.0.0", start: 20000, end: 20099, method: security.MethodAES256, network: NetworkTCPUDP, field: "public_host"},
		{name: "监听地址非 IP", host: "example.com", listen: "example.com", start: 20000, end: 20099, method: security.MethodAES256, network: NetworkTCPUDP, field: "listen_address"},
		{name: "端口池倒置", host: "example.com", listen: "0.0.0.0", start: 20100, end: 20000, method: security.MethodAES256, network: NetworkTCPUDP, field: "port_pool_end"},
		{name: "端口池越界", host: "example.com", listen: "0.0.0.0", start: 100, end: 20000, method: security.MethodAES256, network: NetworkTCPUDP, field: "port_pool_start"},
		{name: "不支持的加密方式", host: "example.com", listen: "0.0.0.0", start: 20000, end: 20099, method: "chacha20", network: NetworkTCPUDP, field: "method"},
		{name: "非法网络能力", host: "example.com", listen: "0.0.0.0", start: 20000, end: 20099, method: security.MethodAES256, network: Network("quic"), field: "network"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewInboundTemplate(id, instance, "Main", test.host, test.listen, test.start, test.end, test.method, test.network, now)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Field != test.field {
				t.Fatalf("err = %v, want field %s", err, test.field)
			}
		})
	}
}

// 入站模板不再持有服务端密钥与 bootstrap 身份：每条专属入站的密钥由面板独立生成（FR-004）。
func TestInboundTemplateHasNoServerKeyOrBootstrap(t *testing.T) {
	id, _ := NewID()
	instance, _ := NewID()
	template, err := NewInboundTemplate(id, instance, "Main", "example.com", "127.0.0.1", 20000, 20000,
		security.MethodAES128, NetworkTCP, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if template.Pool.Capacity() != 1 {
		t.Fatalf("单端口池容量 = %d", template.Pool.Capacity())
	}
	if err := template.ApplyCompatibility(CompatibilityCompatible, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if template.Compatibility != CompatibilityCompatible || template.LastValidatedAt == nil {
		t.Fatalf("compatibility = %#v", template)
	}
}
