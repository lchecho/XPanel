package xray_test

import (
	"context"
	"errors"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 契约门禁 2/3/6/10：面板在运行期创建 SS2022 多用户入站，端口在返回后立即监听、
// GetInboundUsersCount > 0、可在其上增删用户；移除后端口释放；命名空间之外的入站全程只读且不变。
func TestLiveInboundLifecycleContract(t *testing.T) {
	runtime := startRuntime(t)
	tag, port := panelTag("lifecycle"), freePort(t)
	createInbound(t, runtime, tag, port, testKey('u'))

	// 门禁 2：AddInbound 返回后端口立即可连接。
	if !listening(port) {
		t.Fatalf("port %d is not listening right after AddInbound\n%s", port, runtime.diagnostics())
	}
	// 门禁 3：入站带有恰好一个受管客户端，且面板能读到客户端数量。
	inbounds, err := runtime.client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var created, operator *ports.RemoteInbound
	for i := range inbounds {
		switch inbounds[i].InboundTag {
		case tag:
			created = &inbounds[i]
		case operatorInboundTag:
			operator = &inbounds[i]
		}
	}
	if created == nil || !created.PanelManaged || created.UserCount != 1 {
		t.Fatalf("created inbound = %#v", created)
	}
	// 门禁 10：运维自有入站可见但不属于面板命名空间，面板不读取其客户端数量。
	if operator == nil || operator.PanelManaged || operator.UserCount != 0 {
		t.Fatalf("operator inbound = %#v", operator)
	}

	// 门禁 3：可在创建出的入站上增删用户。
	runtimeInbound := ports.RuntimeInbound{InboundTag: tag, Method: security.MethodAES256}
	extra := ports.AddUserCommand{InboundTag: tag, StatisticsID: panelTag("extra-client"), CredentialVersion: 1,
		UserKey: security.NewRedactedString(testKey('x'))}
	if _, err := runtime.client.AddUser(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	users, err := runtime.client.ListUsers(context.Background(), runtimeInbound)
	if err != nil || len(users) != 2 {
		t.Fatalf("users after add = %#v, %v", users, err)
	}
	if _, err := runtime.client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: tag, StatisticsID: extra.StatisticsID}); err != nil {
		t.Fatal(err)
	}

	// 重复标签被拒绝；错误类别稳定，供同步器按已收敛/补偿分流。
	_, duplicateErr := runtime.client.CreateInbound(context.Background(), ports.CreateInboundCommand{InboundTag: tag,
		ListenAddress: listenAddress, Port: freePort(t), Method: security.MethodAES256, Network: domain.NetworkTCPUDP,
		ServerKey: security.NewRedactedString(testKey('s')),
		Client:    ports.InboundClient{StatisticsID: panelTag("dup-client"), CredentialVersion: 1, UserKey: security.NewRedactedString(testKey('d'))}})
	var adapterErr *ports.AdapterError
	if !errors.As(duplicateErr, &adapterErr) || adapterErr.Kind != ports.ErrorInboundAlreadyExists {
		t.Fatalf("duplicate tag error = %v", duplicateErr)
	}

	// 门禁 6/契约 4：移除整条入站后端口释放。
	if _, err := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: tag}); err != nil {
		t.Fatal(err)
	}
	if listening(port) {
		t.Fatalf("port %d is still listening after RemoveInbound", port)
	}
	_, missingErr := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: tag})
	if !errors.As(missingErr, &adapterErr) || adapterErr.Kind != ports.ErrorInboundNotFound {
		t.Fatalf("removing a missing inbound = %v", missingErr)
	}

	// 门禁 10：面板拒绝移除命名空间之外的入站，运维入站始终在监听。
	_, refused := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: operatorInboundTag})
	if !errors.As(refused, &adapterErr) || adapterErr.Kind != ports.ErrorInvalidArgument {
		t.Fatalf("removing an inbound outside the namespace = %v", refused)
	}
	if !listening(runtime.operatorPort) {
		t.Fatal("operator inbound stopped listening")
	}
}

// 契约门禁 9：两条专属入站相互隔离——对其中一条的创建、增删用户与移除不影响另一条的监听。
func TestLiveInboundsAreIsolatedFromEachOther(t *testing.T) {
	runtime := startRuntime(t)
	firstTag, firstPort := panelTag("first"), freePort(t)
	secondTag, secondPort := panelTag("second"), freePort(t)
	createInbound(t, runtime, firstTag, firstPort, testKey('a'))
	createInbound(t, runtime, secondTag, secondPort, testKey('b'))
	if !listening(firstPort) || !listening(secondPort) {
		t.Fatalf("ports not listening: first=%v second=%v", listening(firstPort), listening(secondPort))
	}
	if _, err := runtime.client.RemoveInbound(context.Background(), ports.RemoveInboundCommand{InboundTag: firstTag}); err != nil {
		t.Fatal(err)
	}
	if listening(firstPort) || !listening(secondPort) {
		t.Fatalf("isolation broken after removal: first=%v second=%v", listening(firstPort), listening(secondPort))
	}
	users, err := runtime.client.ListUsers(context.Background(), ports.RuntimeInbound{InboundTag: secondTag, Method: security.MethodAES256})
	if err != nil || len(users) != 1 {
		t.Fatalf("second inbound users = %#v, %v", users, err)
	}
}
