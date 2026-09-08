package xray_test

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 真实故障契约：Xray 不接受同 email 的重复添加，也不能原地替换密钥，因此轮换必须先删后加；
// 而面板的适配器拒绝「移除最后一个受管客户端」，所以先删后加必须先放一个过渡客户端（FR-019）。
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

	// 2) 直接删掉唯一的受管客户端被适配器拒绝——这条守卫是 FR-019 的最终防线，
	//    它保证任何路径（包括租约竞态下的过期请求）都无法把入站清空。
	_, err = runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
		StatisticsID: statisticsID, ExpectedStatisticsID: statisticsID})
	if !errors.As(err, &adapterErr) || adapterErr.Kind != ports.ErrorLastManagedClient {
		t.Fatalf("removing the only client = %v (want last_managed_client)", err)
	}
	users, err := runtime.client.ListUsers(context.Background(), inbound)
	if err != nil || len(users) != 1 {
		t.Fatalf("the refused removal changed the inbound: %#v, %v", users, err)
	}
	if !listening(port) {
		t.Fatalf("port %d stopped listening after a refused removal", port)
	}

	// 3) 停止访问仍然是移除整条入站，端口随之释放，且可用同一端口重建。
	if _, err := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: tag}); err != nil {
		t.Fatal(err)
	}
	if listening(port) {
		t.Fatalf("port %d was not released by the inbound removal", port)
	}
	createInbound(t, runtime, tag, port, testKey('w'))
	if !listening(port) {
		t.Fatalf("port %d did not come back after the rebuild", port)
	}
}

// T084 契约：轮换的四步过渡在真实 Xray 上，任一步之后中断都不会让入站失去受管客户端，
// 端口全程可连接；从任一中间状态续跑都能收敛到「恰好一个受管客户端」。
func TestLiveRotationTransitionKeepsAtLeastOneClientAtEveryBoundary(t *testing.T) {
	runtime := startRuntime(t)
	tag, port := panelTag("transition"), freePort(t)
	createInbound(t, runtime, tag, port, testKey('u'))
	inbound := ports.RuntimeInbound{InboundTag: tag, Method: security.MethodAES256}
	expected := panelTag("transition-client")
	safety := expected + domain.RotationSuffix

	clients := func(step string) map[string]bool {
		users, err := runtime.client.ListUsers(context.Background(), inbound)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if len(users) == 0 {
			t.Fatalf("%s: inbound has no managed client", step)
		}
		if !listening(port) {
			t.Fatalf("%s: port %d stopped listening", step, port)
		}
		present := map[string]bool{}
		for _, user := range users {
			present[user.StatisticsID] = user.Present
		}
		return present
	}
	add := func(step, id string, fill byte) {
		if _, err := runtime.client.AddUser(context.Background(), ports.AddUserCommand{InboundTag: tag, StatisticsID: id,
			CredentialVersion: 2, UserKey: security.NewRedactedString(testKey(fill))}); err != nil {
			t.Fatalf("%s: %v\n%s", step, err, runtime.diagnostics())
		}
	}
	remove := func(step, id string) {
		if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag,
			StatisticsID: id, ExpectedStatisticsID: expected}); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	clients("start")
	add("step 1 add transition", safety, 's')
	if state := clients("after step 1"); !state[expected] || !state[safety] {
		t.Fatalf("state after step 1 = %#v", state)
	}
	remove("step 2 remove old", expected)
	if state := clients("after step 2"); state[expected] || !state[safety] {
		t.Fatalf("state after step 2 = %#v", state)
	}
	add("step 3 add new", expected, 'v')
	if state := clients("after step 3"); !state[expected] || !state[safety] {
		t.Fatalf("state after step 3 = %#v", state)
	}
	remove("step 4 remove transition", safety)
	final := clients("after step 4")
	if !final[expected] || final[safety] || len(final) != 1 {
		t.Fatalf("final state = %#v, want exactly the managed identity", final)
	}

	// 从「只剩过渡客户端」这个最危险的中间状态续跑，同样收敛且全程有客户端。
	add("resume: add transition", safety, 't')
	remove("resume: remove expected", expected)
	if state := clients("resume: only the transition client"); state[expected] || !state[safety] {
		t.Fatalf("resume intermediate state = %#v", state)
	}
	add("resume: add expected", expected, 'w')
	remove("resume: drop transition", safety)
	if state := clients("resume: converged"); !state[expected] || len(state) != 1 {
		t.Fatalf("resumed state = %#v", state)
	}
}
