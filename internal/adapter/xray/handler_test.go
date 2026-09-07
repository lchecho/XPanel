package xray

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	handlercommand "github.com/xtls/xray-core/app/proxyman/command"
	statscommand "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	shadowsocks2022 "github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

type handlerStub struct {
	users []*protocol.User
	handlercommand.UnimplementedHandlerServiceServer
	mu       sync.Mutex
	last     *handlercommand.AlterInboundRequest
	alterErr error
	delay    time.Duration
}

func (s *handlerStub) ListInbounds(context.Context, *handlercommand.ListInboundsRequest) (*handlercommand.ListInboundsResponse, error) {
	config := &shadowsocks2022.MultiUserServerConfig{Method: security.MethodAES256}
	return &handlercommand.ListInboundsResponse{Inbounds: []*core.InboundHandlerConfig{{Tag: "managed", ProxySettings: serial.ToTypedMessage(config)}}}, nil
}

func (s *handlerStub) AddInbound(context.Context, *handlercommand.AddInboundRequest) (*handlercommand.AddInboundResponse, error) {
	return &handlercommand.AddInboundResponse{}, nil
}

func (s *handlerStub) RemoveInbound(context.Context, *handlercommand.RemoveInboundRequest) (*handlercommand.RemoveInboundResponse, error) {
	return &handlercommand.RemoveInboundResponse{}, nil
}

func (s *handlerStub) GetInboundUsersCount(context.Context, *handlercommand.GetInboundUserRequest) (*handlercommand.GetInboundUsersCountResponse, error) {
	return &handlercommand.GetInboundUsersCountResponse{Count: 2}, nil
}

func (s *handlerStub) GetInboundUsers(context.Context, *handlercommand.GetInboundUserRequest) (*handlercommand.GetInboundUserResponse, error) {
	if s.users != nil {
		return &handlercommand.GetInboundUserResponse{Users: s.users}, nil
	}
	return &handlercommand.GetInboundUserResponse{Users: []*protocol.User{{Email: "bootstrap"},
		{Email: "xpanel-managed-inbound"}, {Email: "foreign"}}}, nil
}

func (s *handlerStub) AlterInbound(ctx context.Context, request *handlercommand.AlterInboundRequest) (*handlercommand.AlterInboundResponse, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	s.last = request
	err := s.alterErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &handlercommand.AlterInboundResponse{}, nil
}

type statsStub struct {
	statscommand.UnimplementedStatsServiceServer
	mu       sync.Mutex
	requests []*statscommand.GetStatsRequest
}

func (s *statsStub) GetSysStats(context.Context, *statscommand.SysStatsRequest) (*statscommand.SysStatsResponse, error) {
	return &statscommand.SysStatsResponse{Uptime: 42}, nil
}

func (s *statsStub) GetStats(_ context.Context, request *statscommand.GetStatsRequest) (*statscommand.GetStatsResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return &statscommand.GetStatsResponse{Stat: &statscommand.Stat{Name: request.Name, Value: 0}}, nil
}

func newBufClient(t *testing.T, handler *handlerStub, stats *statsStub, timeout time.Duration) *Client {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	handlercommand.RegisterHandlerServiceServer(server, handler)
	statscommand.RegisterStatsServiceServer(server, stats)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewWithConnection(conn, timeout)
}

func TestHandlerContractEncodingAndCapabilities(t *testing.T) {
	handler, stats := &handlerStub{}, &statsStub{}
	client := newBufClient(t, handler, stats, time.Second)
	fixed := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return fixed }

	observation, err := client.Probe(context.Background(), ports.InstanceTarget{})
	if err != nil || observation.UptimeSeconds != 42 || !observation.BootEpoch.Equal(fixed.Add(-42*time.Second)) {
		t.Fatalf("probe = %#v, %v", observation, err)
	}
	// ValidateTemplate 通过一次性探针入站证明能力；stub 接受创建、移除与计数查询，
	// 但它不是真的 SS2022 监听器，因此「用户级统计可读」这一项只能由真实 Xray 契约测试证明
	// （tests/contract/xray/template_test.go）。这里断言前三项能力与「卡在流量探测」这一事实。
	capabilities, err := client.ValidateTemplate(context.Background(), ports.TemplateProbe{ListenAddress: "127.0.0.1",
		ProbePort: 39999, Method: security.MethodAES256, Network: domain.NetworkTCPUDP})
	if err == nil {
		t.Fatalf("stub without a real listener unexpectedly completed the traffic probe: %#v", capabilities)
	}
	if !capabilities.InboundCreatable || !capabilities.InboundRemovable || !capabilities.MultiUserSupported ||
		!capabilities.MethodSupported || capabilities.TrafficAccounted {
		t.Fatalf("capabilities before the traffic probe = %#v", capabilities)
	}
	if capabilities.ProbeStatisticsID == "" || !domain.IsPanelNamespace(capabilities.ProbeStatisticsID) {
		t.Fatalf("probe identity is not observable: %q", capabilities.ProbeStatisticsID)
	}
	// 网络能力缺失是调用错误，必须直接报错，而不是记成「节点不兼容」。
	if _, err := client.ValidateTemplate(context.Background(), ports.TemplateProbe{ListenAddress: "127.0.0.1",
		ProbePort: 39998, Method: security.MethodAES256}); err == nil {
		t.Fatal("a probe without a network selection was accepted")
	}

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	command := ports.AddUserCommand{InboundTag: "xpanel-managed-inbound", StatisticsID: "xpanel-managed", CredentialVersion: 7,
		UserKey: security.NewRedactedString(key)}
	if _, err := client.AddUser(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	instance, err := handler.last.GetOperation().GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	add, ok := instance.(*handlercommand.AddUserOperation)
	if !ok || add.GetUser().GetEmail() != command.StatisticsID || add.GetUser().GetLevel() != 0 {
		t.Fatalf("add operation = %#v", instance)
	}
	accountMessage, err := add.GetUser().GetAccount().GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	account, ok := accountMessage.(*shadowsocks2022.Account)
	if !ok || account.GetKey() != key {
		t.Fatalf("account type/value = %T", accountMessage)
	}
	// 入站标签与期望统计标识由同一个分配标识派生，因此恒等；轮换过渡身份在其后加 -rotate。
	// 过渡身份在场时，移除期望身份是允许的（轮换第 2 步）。
	handler.users = []*protocol.User{{Email: "bootstrap"}, {Email: "xpanel-managed-inbound"},
		{Email: "xpanel-managed-inbound-rotate"}, {Email: "foreign"}}
	if _, err := client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: "xpanel-managed-inbound",
		StatisticsID: "xpanel-managed-inbound"}); err != nil {
		t.Fatal(err)
	}
	instance, err = handler.last.GetOperation().GetInstance()
	remove, ok := instance.(*handlercommand.RemoveUserOperation)
	if err != nil || !ok || remove.GetEmail() != "xpanel-managed-inbound" {
		t.Fatalf("remove operation = %#v, %v", instance, err)
	}

	// 有效后继只认「期望身份 + 它的轮换过渡身份」这对精确配对。以下三种残留都不算「还有人」：
	for _, leftover := range []string{"foreign", "xpanel-someone-else", "xpanel-managed-inbound-rotate-extra"} {
		handler.users = []*protocol.User{{Email: "xpanel-managed-inbound"}, {Email: leftover}}
		_, lastErr := client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: "xpanel-managed-inbound",
			StatisticsID: "xpanel-managed-inbound"})
		var lastAdapterErr *ports.AdapterError
		if !errors.As(lastErr, &lastAdapterErr) || lastAdapterErr.Kind != ports.ErrorLastManagedClient {
			t.Fatalf("removing the managed client while only %q remains = %v", leftover, lastErr)
		}
	}
	// 反过来，清理未知身份是允许的——期望身份还在。
	handler.users = []*protocol.User{{Email: "xpanel-managed-inbound"}, {Email: "foreign"}}
	if _, err := client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: "xpanel-managed-inbound",
		StatisticsID: "foreign"}); err != nil {
		t.Fatalf("removing an unknown identity while the managed client is present: %v", err)
	}
	handler.users = nil

	// 命名空间之外的入站对面板只读：变更在到达 Xray 之前就被拒绝。
	before := handler.last
	if _, err := client.AddUser(context.Background(), ports.AddUserCommand{InboundTag: "operator-inbound",
		StatisticsID: "xpanel-intruder", UserKey: security.NewRedactedString(key)}); err == nil {
		t.Fatal("panel mutated an inbound outside its namespace")
	}
	if _, err := client.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: "operator-inbound",
		StatisticsID: "xpanel-intruder"}); err == nil {
		t.Fatal("panel removed a client from an inbound outside its namespace")
	}
	if handler.last != before {
		t.Fatal("a refused mutation still reached Xray")
	}

	// 面板命名空间前缀是区分受管与外部身份的唯一依据；bootstrap 概念已随共享入站模型退役。
	users, err := client.ListUsers(context.Background(), ports.RuntimeInbound{InboundTag: "xpanel-managed-inbound", Method: security.MethodAES256})
	if err != nil || len(users) != 3 {
		t.Fatalf("users = %#v, %v", users, err)
	}
	kinds := map[string]string{}
	for _, user := range users {
		kinds[user.StatisticsID] = user.Kind
	}
	if kinds["xpanel-managed-inbound"] != "managed" || kinds["bootstrap"] != "external" || kinds["foreign"] != "external" {
		t.Fatalf("identity kinds = %#v", kinds)
	}
}

func TestHandlerStableErrorsDoNotLeakKeys(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	handler := &handlerStub{alterErr: status.Error(codes.AlreadyExists, "duplicate "+key)}
	client := newBufClient(t, handler, &statsStub{}, time.Second)
	_, err := client.AddUser(context.Background(), ports.AddUserCommand{InboundTag: "xpanel-managed-inbound", StatisticsID: "xpanel-user",
		UserKey: security.NewRedactedString(key)})
	adapterErr, ok := err.(*ports.AdapterError)
	if !ok || adapterErr.Kind != ports.ErrorUserAlreadyExists || strings.Contains(err.Error(), key) {
		t.Fatalf("unsafe or unstable error: %T %v", err, err)
	}

	timeoutClient := newBufClient(t, &handlerStub{delay: 100 * time.Millisecond}, &statsStub{}, 5*time.Millisecond)
	_, err = timeoutClient.RemoveUser(context.Background(), ports.RemoveUserCommand{InboundTag: "xpanel-managed-inbound", StatisticsID: "xpanel-user"})
	adapterErr, ok = err.(*ports.AdapterError)
	if !ok || adapterErr.Kind != ports.ErrorDeadlineExceeded || !adapterErr.Retryable {
		t.Fatalf("deadline mapping = %T %v", err, err)
	}
}
