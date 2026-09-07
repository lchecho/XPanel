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

func (c *Client) AddUser(ctx context.Context, command ports.AddUserCommand) (ports.MutationReceipt, error) {
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

func (c *Client) RemoveUser(ctx context.Context, command ports.RemoveUserCommand) (ports.MutationReceipt, error) {
	operation := &handlercommand.RemoveUserOperation{Email: command.StatisticsID}
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	_, err := c.handler.AlterInbound(callCtx, &handlercommand.AlterInboundRequest{Tag: command.InboundTag, Operation: serial.ToTypedMessage(operation)})
	if err != nil {
		return ports.MutationReceipt{}, mapError("remove_user", err)
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: c.now().UTC()}, nil
}
