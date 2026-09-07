package domain

import (
	"errors"
	"testing"
)

func TestInboundNamespaceAndTagValidation(t *testing.T) {
	id := ID("11111111-2222-4333-8444-555555555555")
	tag := InboundTag(id)
	if tag != "xpanel-"+id.String() {
		t.Fatalf("tag = %q", tag)
	}
	if !IsPanelNamespace(tag) || IsPanelNamespace("managed") || IsPanelNamespace("Xpanel-x") {
		t.Fatal("namespace detection is wrong")
	}
	tests := []struct {
		name    string
		tag     string
		wantErr bool
	}{
		{name: "派生标签", tag: tag},
		{name: "缺少前缀", tag: "managed", wantErr: true},
		{name: "仅前缀", tag: "xpanel-", wantErr: true},
		{name: "含统计名分隔符", tag: "xpanel-a>>>b", wantErr: true},
		{name: "含控制字符", tag: "xpanel-a\nb", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateInboundTag(test.tag)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateInboundTag(%q) err = %v", test.tag, err)
			}
			if test.wantErr {
				var invalid *ValidationError
				if !errors.As(err, &invalid) {
					t.Fatalf("error type = %T, want *ValidationError", err)
				}
			}
		})
	}
}

// 期望状态真值表：生命周期、管理员意图与配额状态三者全部成立才监听。
func TestInboundDesiredPresentTruthTable(t *testing.T) {
	tests := []struct {
		lifecycle LifecycleState
		enabled   bool
		quota     QuotaState
		want      bool
	}{
		{LifecycleActive, true, QuotaWithinLimit, true},
		{LifecycleActive, false, QuotaWithinLimit, false},
		{LifecycleActive, true, QuotaExceeded, false},
		{LifecycleActive, false, QuotaExceeded, false},
		{LifecycleDeleted, true, QuotaWithinLimit, false},
		{LifecycleDeleted, false, QuotaExceeded, false},
	}
	for _, test := range tests {
		if got := InboundDesiredPresent(test.lifecycle, test.enabled, test.quota); got != test.want {
			t.Fatalf("InboundDesiredPresent(%s,%v,%s) = %v want %v", test.lifecycle, test.enabled, test.quota, got, test.want)
		}
	}
	// 必须与 AccessAllocation.DesiredPresent 保持同一真值表。
	for _, test := range tests {
		allocation := AccessAllocation{AdminEnabled: test.enabled, QuotaState: test.quota}
		user := ManagedUser{Lifecycle: test.lifecycle}
		if allocation.DesiredPresent(user) != InboundDesiredPresent(test.lifecycle, test.enabled, test.quota) {
			t.Fatalf("入站期望状态与分配期望状态不一致: %#v", test)
		}
	}
}

func TestInboundMustCarryExactlyOneClient(t *testing.T) {
	if err := ValidateInboundClients(1); err != nil {
		t.Fatalf("one client rejected: %v", err)
	}
	for _, count := range []int{0, 2, 5} {
		err := ValidateInboundClients(count)
		var invalid *InvalidStateError
		if !errors.As(err, &invalid) {
			t.Fatalf("ValidateInboundClients(%d) err = %v, want *InvalidStateError", count, err)
		}
	}
}

func TestDedicatedInboundAssigned(t *testing.T) {
	inbound := DedicatedInbound{}
	if !inbound.Assigned() {
		t.Fatal("未释放的入站应持有端口")
	}
	released := nowPointer()
	inbound.ReleasedAt = released
	if inbound.Assigned() {
		t.Fatal("已释放的入站不应再持有端口")
	}
}
