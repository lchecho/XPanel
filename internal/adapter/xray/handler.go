package xray

import (
	"context"
	"encoding/base64"
	"time"

	handlercommand "github.com/xtls/xray-core/app/proxyman/command"
	statscommand "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	shadowsocks2022 "github.com/xtls/xray-core/proxy/shadowsocks_2022"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

func (c *Client) Probe(ctx context.Context, _ ports.InstanceTarget) (ports.InstanceObservation, error) {
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	response, err := c.stats.GetSysStats(callCtx, &statscommand.SysStatsRequest{})
	if err != nil {
		return ports.InstanceObservation{}, mapError("probe", err)
	}
	now := c.now().UTC()
	// uptime 是 uint32 整秒，推算出的 boot epoch 在相邻观察之间最多抖动一秒；观察者按 UptimeSeconds 的单调性
	// 与一秒容差区分量化抖动与真实重启（application.observationMismatch / domain.RestartConfirmed）。
	return ports.InstanceObservation{ObservedAt: now, UptimeSeconds: response.GetUptime(),
		BootEpoch: now.Truncate(time.Second).Add(-timeDurationSeconds(response.GetUptime())), BootEpochKnown: true}, nil
}

func timeDurationSeconds(seconds uint32) time.Duration { return time.Duration(seconds) * time.Second }

// guardPanelInbound 拒绝对面板保留命名空间之外的入站执行任何变更。
// AI-LOCK：面板只管理自己创建的入站，运维自有入站在全流程中必须保持不变（宪章 I/II）。
func guardPanelInbound(operation, tag string) error {
	if domain.IsPanelNamespace(tag) {
		return nil
	}
	return &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: operation,
		SafeSummary: "refusing to mutate an inbound outside the panel namespace"}
}

func (c *Client) ListUsers(ctx context.Context, inbound ports.RuntimeInbound) ([]ports.RemoteUser, error) {
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	response, err := c.handler.GetInboundUsers(callCtx, &handlercommand.GetInboundUserRequest{Tag: inbound.InboundTag})
	if err != nil {
		return nil, mapError("list_users", err)
	}
	users := make([]ports.RemoteUser, 0, len(response.GetUsers()))
	for _, user := range response.GetUsers() {
		kind := "external"
		if domain.IsPanelNamespace(user.GetEmail()) {
			kind = "managed"
		}
		users = append(users, ports.RemoteUser{StatisticsID: user.GetEmail(), Present: true, Kind: kind})
	}
	return users, nil
}

// AddUser 在面板专属入站内添加受管客户端。
// 约束：只作用于面板保留命名空间内的入站；运维自有入站对面板只读（宪章 I/II）。
func (c *Client) AddUser(ctx context.Context, command ports.AddUserCommand) (ports.MutationReceipt, error) {
	if err := guardPanelInbound("add_user", command.InboundTag); err != nil {
		return ports.MutationReceipt{}, err
	}
	if err := security.ValidateUserKey(methodFromKey(command.UserKey.Reveal()), command.UserKey.Reveal()); err != nil {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "add_user", SafeSummary: "invalid user key"}
	}
	operation := &handlercommand.AddUserOperation{User: &protocol.User{Level: 0, Email: command.StatisticsID,
		Account: serial.ToTypedMessage(&shadowsocks2022.Account{Key: command.UserKey.Reveal()})}}
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	_, err := c.handler.AlterInbound(callCtx, &handlercommand.AlterInboundRequest{Tag: command.InboundTag, Operation: serial.ToTypedMessage(operation)})
	if err != nil {
		return ports.MutationReceipt{}, mapError("add_user", err)
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: c.now().UTC()}, nil
}

func methodFromKey(encoded string) string {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err == nil && len(decoded) == 16 {
		return security.MethodAES128
	}
	return security.MethodAES256
}

// RemoveUser 移除面板专属入站内的一个客户端；限定在面板命名空间内，且不得移除最后一个受管客户端。
//
// AI-LOCK：入站的**受管**客户端数在任何时刻都不得为 0（FR-019）。判断依据只能是「移除之后是否
// 还留有面板命名空间内的客户端」——不能用总用户数，否则一条被注入了外部身份的入站会被误判为
// 「还有人」而放行，结果是入站只剩那个未知身份还在对外服务。期望身份缺失时正确的顺序是
// 先恢复期望客户端再清理未知身份，因此这里直接拒绝，由协调器重建。
// 这条守卫与租约无关，是该不变量的最终防线：即便竞态让一个过期的移除请求漏到这里也不会生效。
func (c *Client) RemoveUser(ctx context.Context, command ports.RemoveUserCommand) (ports.MutationReceipt, error) {
	if err := guardPanelInbound("remove_user", command.InboundTag); err != nil {
		return ports.MutationReceipt{}, err
	}
	// 缺少期望身份是调用方的错误：没有它就无法判断「移除后是否还留有受管客户端」，
	// 只能拒绝，不能退化成「凭标签猜」或「不检查」（T097）。
	if command.ExpectedStatisticsID == "" {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorInvalidArgument, Operation: "remove_user",
			SafeSummary: "remove_user requires the inbound's expected managed identity"}
	}
	// 精确判定：只有「目标确实在，且移除后不再有任何受管客户端」才拒绝。
	// 移除本就不存在的客户端不改变任何数量，照常返回 user_not_found 让调用方按已收敛处理。
	listCtx, listCancel := c.deadline(ctx)
	list, listErr := c.handler.GetInboundUsers(listCtx, &handlercommand.GetInboundUserRequest{Tag: command.InboundTag})
	listCancel()
	if listErr != nil {
		return ports.MutationReceipt{}, mapError("remove_user", listErr)
	}
	// 有效后继只有两个：命令显式给出的期望身份，以及由它派生的轮换过渡身份。
	// 任意带 xpanel- 前缀的身份都不算——那可能是别的分配的身份或残留的未知身份，
	// 把它当成「还有人」等于放任入站被顶替（T092/T097）。
	expected := command.ExpectedStatisticsID
	safety := domain.RotationSafetyID(expected)
	present, survivor := false, false
	for _, user := range list.GetUsers() {
		email := user.GetEmail()
		if email == command.StatisticsID {
			present = true
			continue
		}
		if email == expected || email == safety {
			survivor = true
		}
	}
	if present && !survivor {
		return ports.MutationReceipt{}, &ports.AdapterError{Kind: ports.ErrorLastManagedClient, Operation: "remove_user",
			Retryable: false, SafeSummary: "refusing to remove the last managed client of an inbound"}
	}
	operation := &handlercommand.RemoveUserOperation{Email: command.StatisticsID}
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	_, err := c.handler.AlterInbound(callCtx, &handlercommand.AlterInboundRequest{Tag: command.InboundTag, Operation: serial.ToTypedMessage(operation)})
	if err != nil {
		return ports.MutationReceipt{}, mapError("remove_user", err)
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: c.now().UTC()}, nil
}
