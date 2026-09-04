package xray

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	handlercommand "github.com/xtls/xray-core/app/proxyman/command"
	statscommand "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	shadowsocks2022 "github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	return ports.InstanceObservation{ObservedAt: now, UptimeSeconds: response.GetUptime(),
		BootEpoch: now.Add(-timeDurationSeconds(response.GetUptime())).Truncate(time.Second), BootEpochKnown: true}, nil
}

func timeDurationSeconds(seconds uint32) time.Duration { return time.Duration(seconds) * time.Second }

func (c *Client) ValidateProfile(ctx context.Context, profile ports.RuntimeProfile) (ports.ProfileCapabilities, error) {
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	list, err := c.handler.ListInbounds(callCtx, &handlercommand.ListInboundsRequest{IsOnlyTags: false})
	if err != nil {
		return ports.ProfileCapabilities{}, mapError("validate_profile", err)
	}
	capabilities := ports.ProfileCapabilities{}
	for _, inbound := range list.GetInbounds() {
		if inbound.GetTag() == profile.InboundTag {
			capabilities.InboundPresent = true
			if inbound.GetProxySettings() != nil {
				instance, decodeErr := inbound.GetProxySettings().GetInstance()
				if decodeErr != nil {
					capabilities.CompatibilityReason = "inbound protocol settings could not be decoded"
					return capabilities, nil
				}
				switch config := instance.(type) {
				case *shadowsocks2022.MultiUserServerConfig:
					capabilities.ProtocolSupported = true
					capabilities.MethodSupported = config.GetMethod() == profile.Method &&
						(profile.Method == security.MethodAES128 || profile.Method == security.MethodAES256)
				case *shadowsocks2022.ServerConfig:
					capabilities.ProtocolSupported = true
					capabilities.MethodSupported = config.GetMethod() == profile.Method
				}
			}
			break
		}
	}
	if !capabilities.InboundPresent {
		capabilities.CompatibilityReason = "configured inbound was not found"
		return capabilities, nil
	}
	count, err := c.handler.GetInboundUsersCount(callCtx, &handlercommand.GetInboundUserRequest{Tag: profile.InboundTag})
	if err != nil {
		mapped := mapError("validate_profile", err)
		var adapterErr *ports.AdapterError
		if errors.As(mapped, &adapterErr) && adapterErr.Kind == ports.ErrorIncompatibleProfile {
			capabilities.CompatibilityReason = adapterErr.SafeSummary
			return capabilities, nil
		}
		return capabilities, mapped
	}
	capabilities.MultiUserSupported = count.GetCount() > 0
	users, err := c.handler.GetInboundUsers(callCtx, &handlercommand.GetInboundUserRequest{Tag: profile.InboundTag})
	if err != nil {
		return capabilities, mapError("validate_profile", err)
	}
	for _, user := range users.GetUsers() {
		if user.GetEmail() == profile.BootstrapStatisticsID {
			capabilities.BootstrapVisible = true
			break
		}
	}
	capabilities.IndependentStats = capabilities.BootstrapVisible
	if capabilities.BootstrapVisible {
		for _, direction := range []ports.Direction{ports.Uplink, ports.Downlink} {
			name, nameErr := CounterName(profile.BootstrapStatisticsID, direction)
			if nameErr != nil {
				return capabilities, nameErr
			}
			_, statErr := c.stats.GetStats(callCtx, &statscommand.GetStatsRequest{Name: name, Reset_: false})
			if statErr != nil && status.Code(statErr) != codes.NotFound {
				return capabilities, mapError("validate_profile", statErr)
			}
		}
	}
	if !capabilities.Compatible() {
		capabilities.CompatibilityReason = "inbound does not satisfy the Shadowsocks 2022 multi-user contract"
	}
	return capabilities, nil
}

func (c *Client) ListUsers(ctx context.Context, profile ports.RuntimeProfile) ([]ports.RemoteUser, error) {
	callCtx, cancel := c.deadline(ctx)
	defer cancel()
	response, err := c.handler.GetInboundUsers(callCtx, &handlercommand.GetInboundUserRequest{Tag: profile.InboundTag})
	if err != nil {
		return nil, mapError("list_users", err)
	}
	users := make([]ports.RemoteUser, 0, len(response.GetUsers()))
	for _, user := range response.GetUsers() {
		kind := "external"
		if user.GetEmail() == profile.BootstrapStatisticsID {
			kind = "bootstrap"
		} else if strings.HasPrefix(user.GetEmail(), "xpanel-") {
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
	_, err := c.handler.AlterInbound(callCtx, &handlercommand.AlterInboundRequest{Tag: command.ProfileTag, Operation: serial.ToTypedMessage(operation)})
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
	_, err := c.handler.AlterInbound(callCtx, &handlercommand.AlterInboundRequest{Tag: command.ProfileTag, Operation: serial.ToTypedMessage(operation)})
	if err != nil {
		return ports.MutationReceipt{}, mapError("remove_user", err)
	}
	return ports.MutationReceipt{OperationID: command.OperationID, ObservedAt: c.now().UTC()}, nil
}
