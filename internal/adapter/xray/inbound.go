package xray

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	proxyman "github.com/xtls/xray-core/app/proxyman"
	handlercommand "github.com/xtls/xray-core/app/proxyman/command"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	shadowsocks2022 "github.com/xtls/xray-core/proxy/shadowsocks_2022"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

// 下游调用：Xray HandlerService 的入站生命周期（AddInbound / RemoveInbound / ListInbounds）。
//
// 职责：把面板的专属入站意图翻译为定向 protobuf 并提交；不负责决定何时创建或移除（由 synchronizer 决定）。
// 约束：只用 core/app/proxyman/proxy/shadowsocks_2022/common 的定向类型，MUST NOT 引入 infra/conf
//
//	（research.md R-001，会链入 gvisor/WireGuard 等 53 个无关包）；构建期拒绝空客户端列表（R-005）。
//
// 契约：specs/002-per-user-inbound/contracts/xray-adapter.md
// 幂等：以 inbound_tag 为键；already_exists / not_found 由调用方按已收敛处理
// 失败处理：AddInbound 非原子——bind 失败时入站仍被注册，调用方 MUST 读后写确认并补偿移除（R-003）。
// AI-LOCK：Users 必须恰好一个受管客户端；空列表会让入站退化为服务端密钥可直连的单用户模式。

// buildInboundConfig 用定向 protobuf 构建 SS2022 多用户入站配置。
func buildInboundConfig(command ports.CreateInboundCommand) (*core.InboundHandlerConfig, error) {
	if err := domain.ValidateInboundTag(command.InboundTag); err != nil {
		return nil, err
	}
	if err := domain.ValidateInboundClients(1); err != nil {
		return nil, err
	}
	if command.Client.StatisticsID == "" || !domain.IsPanelNamespace(command.Client.StatisticsID) {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "create_inbound", SafeSummary: "client statistics identity must use the panel namespace"}
	}
	if command.Port < domain.MinAssignablePort || command.Port > domain.MaxAssignablePort {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "create_inbound", SafeSummary: "port is outside the assignable range"}
	}
	if net.ParseIP(command.ListenAddress) == nil {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "create_inbound", SafeSummary: "listen address must be an IP literal"}
	}
	if err := security.ValidateUserKey(command.Method, command.ServerKey.Reveal()); err != nil {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "create_inbound", SafeSummary: "invalid server key for the selected method"}
	}
	if err := security.ValidateUserKey(command.Method, command.Client.UserKey.Reveal()); err != nil {
		return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "create_inbound", SafeSummary: "invalid user key for the selected method"}
	}
	networks, err := networksFor(command.Network)
	if err != nil {
		return nil, err
	}
	port := uint32(command.Port)
	receiver := &proxyman.ReceiverConfig{
		PortList: &xnet.PortList{Range: []*xnet.PortRange{{From: port, To: port}}},
		Listen:   xnet.NewIPOrDomain(xnet.ParseAddress(command.ListenAddress)),
	}
	proxy := &shadowsocks2022.MultiUserServerConfig{
		Method: command.Method,
		Key:    command.ServerKey.Reveal(),
		Users: []*protocol.User{{Level: 0, Email: command.Client.StatisticsID,
			Account: serial.ToTypedMessage(&shadowsocks2022.Account{Key: command.Client.UserKey.Reveal()})}},
		Network: networks,
	}
	return &core.InboundHandlerConfig{Tag: command.InboundTag, ReceiverSettings: serial.ToTypedMessage(receiver),
		ProxySettings: serial.ToTypedMessage(proxy)}, nil
}

func networksFor(network domain.Network) ([]xnet.Network, error) {
	switch network {
	case domain.NetworkTCP:
		return []xnet.Network{xnet.Network_TCP}, nil
	case domain.NetworkUDP:
		return []xnet.Network{xnet.Network_UDP}, nil
	case domain.NetworkTCPUDP:
		return []xnet.Network{xnet.Network_TCP, xnet.Network_UDP}, nil
	}
	return nil, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "create_inbound", SafeSummary: "unsupported network selection"}
}

// probePort 在创建前尝试绑定目标端口并立即释放：把绝大多数外部占用转化为创建前的明确拒绝。
// 存在 TOCTOU 窗口，不作为唯一保证——AddInbound 的 bind 错误是第二道检测（research.md R-002）。
func probePort(listenAddress string, port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(listenAddress, strconv.Itoa(port)))
	if err != nil {
		return &ports.AdapterError{Kind: ports.ErrorPortUnavailable, Operation: "create_inbound", Retryable: false,
			SafeSummary: "port is already in use on the node"}
	}
	return listener.Close()
}

// CreateInbound 创建专属入站。返回 port_unavailable 时入站可能仍被 Xray 注册（非原子），调用方负责读后写与补偿。
func (c *Client) CreateInbound(ctx context.Context, command ports.CreateInboundCommand) (ports.MutationReceipt, error) {
	config, err := buildInboundConfig(command)
	if err != nil {
		return ports.MutationReceipt{}, err
	}
	if err := probePort(command.ListenAddress, command.Port); err != nil {
		return ports.MutationReceipt{}, err
	}
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	if _, err := c.handler.AddInbound(callCtx, &handlercommand.AddInboundRequest{Inbound: config}); err != nil {
		return ports.MutationReceipt{}, mapError("create_inbound", err)
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: c.now().UTC()}, nil
}

// RemoveInbound 移除整条入站；不存在视为已收敛，由调用方按 inbound_not_found 处理。
func (c *Client) RemoveInbound(ctx context.Context, command ports.RemoveInboundCommand) (ports.MutationReceipt, error) {
	if !domain.IsPanelNamespace(command.InboundTag) {
		// 命名空间之外的入站对面板只读（宪章 I/II）。
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "remove_inbound",
			SafeSummary: "refusing to remove an inbound outside the panel namespace"}
	}
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	if _, err := c.handler.RemoveInbound(callCtx, &handlercommand.RemoveInboundRequest{Tag: command.InboundTag}); err != nil {
		return ports.MutationReceipt{}, mapError("remove_inbound", err)
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: c.now().UTC()}, nil
}

// ListInbounds 返回 Xray 中全部入站标签；只为面板命名空间内的入站读取客户端数量。
func (c *Client) ListInbounds(ctx context.Context) ([]ports.RemoteInbound, error) {
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	list, err := c.handler.ListInbounds(callCtx, &handlercommand.ListInboundsRequest{IsOnlyTags: true})
	if err != nil {
		return nil, mapError("list_inbounds", err)
	}
	result := make([]ports.RemoteInbound, 0, len(list.GetInbounds()))
	for _, inbound := range list.GetInbounds() {
		remote := ports.RemoteInbound{InboundTag: inbound.GetTag(), PanelManaged: domain.IsPanelNamespace(inbound.GetTag())}
		if remote.PanelManaged {
			count, countErr := c.handler.GetInboundUsersCount(callCtx, &handlercommand.GetInboundUserRequest{Tag: remote.InboundTag})
			if countErr != nil {
				return nil, mapError("list_inbounds", countErr)
			}
			remote.UserCount = count.GetCount()
		}
		result = append(result, remote)
	}
	return result, nil
}

// ValidateTemplate 用一条一次性探针入站证明实例支持运行时入站管理与 SS2022 多用户身份；探针无论成败都会移除。
// 用户级统计是否开启无法经 API 证实，只能在采集阶段作为健康诊断暴露（contracts/config.md）。
func (c *Client) ValidateTemplate(ctx context.Context, probe ports.TemplateProbe) (ports.TemplateCapabilities, error) {
	capabilities := ports.TemplateCapabilities{ProtocolSupported: true}
	capabilities.MethodSupported = probe.Method == security.MethodAES128 || probe.Method == security.MethodAES256
	if !capabilities.MethodSupported {
		capabilities.CompatibilityReason = "unsupported Shadowsocks 2022 method"
		return capabilities, nil
	}
	serverKey, err := security.GenerateUserKey(probe.Method)
	if err != nil {
		return capabilities, err
	}
	userKey, err := security.GenerateUserKey(probe.Method)
	if err != nil {
		return capabilities, err
	}
	tag := fmt.Sprintf("%sprobe-%s", domain.NamespacePrefix, probe.TemplateID.String())
	command := ports.CreateInboundCommand{InboundTag: tag, ListenAddress: probe.ListenAddress, Port: probe.ProbePort,
		Method: probe.Method, Network: domain.NetworkTCPUDP, ServerKey: serverKey,
		Client: ports.InboundClient{StatisticsID: domain.NamespacePrefix + "probe-client", CredentialVersion: 1, UserKey: userKey}}
	_, createErr := c.CreateInbound(ctx, command)
	// 探针入站无论创建结果如何都必须移除：bind 失败时它仍可能被注册（research.md R-003）。
	defer func() {
		_, _ = c.RemoveInbound(context.WithoutCancel(ctx), ports.RemoveInboundCommand{InboundTag: tag})
	}()
	if createErr != nil {
		var adapterErr *ports.AdapterError
		if errors.As(createErr, &adapterErr) && !adapterErr.Retryable {
			capabilities.CompatibilityReason = "node could not create a probe inbound: " + adapterErr.SafeSummary
			return capabilities, nil
		}
		return capabilities, createErr
	}
	capabilities.InboundCreatable = true
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	count, err := c.handler.GetInboundUsersCount(callCtx, &handlercommand.GetInboundUserRequest{Tag: tag})
	if err != nil {
		return capabilities, mapError("validate_template", err)
	}
	capabilities.MultiUserSupported = count.GetCount() > 0
	if !capabilities.Compatible() {
		capabilities.CompatibilityReason = "node does not satisfy the Shadowsocks 2022 multi-user contract"
	}
	return capabilities, nil
}
