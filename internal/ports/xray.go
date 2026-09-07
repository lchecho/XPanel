package ports

import (
	"context"
	"fmt"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/security"
)

// 下游调用：Xray via gRPC。业务层只通过本接口感知 Xray，protobuf 细节封闭在 internal/adapter/xray（宪章 II）。
type Adapter interface {
	Probe(context.Context, InstanceTarget) (InstanceObservation, error)
	ValidateTemplate(context.Context, TemplateProbe) (TemplateCapabilities, error)
	ListInbounds(context.Context) ([]RemoteInbound, error)
	CreateInbound(context.Context, CreateInboundCommand) (MutationReceipt, error)
	RemoveInbound(context.Context, RemoveInboundCommand) (MutationReceipt, error)
	ListUsers(context.Context, RuntimeInbound) ([]RemoteUser, error)
	AddUser(context.Context, AddUserCommand) (MutationReceipt, error)
	RemoveUser(context.Context, RemoveUserCommand) (MutationReceipt, error)
	ReadTraffic(context.Context, TrafficQuery) (TrafficRound, error)
}

type InstanceTarget struct {
	InstanceID      domain.ID
	APIEndpoint     string
	ExpectedVersion string
	RPCTimeout      time.Duration
}

type InstanceObservation struct {
	ObservedAt     time.Time
	UptimeSeconds  uint32
	BootEpoch      time.Time
	BootEpochKnown bool
}

// RuntimeInbound 标识 Xray 中的一条入站，用于读取其用户列表与增删客户端。
type RuntimeInbound struct {
	TemplateID domain.ID
	InboundTag string
	Method     string
}

// TemplateProbe 描述一次入站模板能力校验：面板会在 ProbePort 上创建一条一次性入站再移除，
// 以证明该实例确实支持运行时入站管理与 SS2022 多用户身份。
type TemplateProbe struct {
	TemplateID    domain.ID
	ListenAddress string
	ProbePort     int
	Method        string
}

// InboundClient 是专属入站内唯一的受管客户端。
type InboundClient struct {
	StatisticsID      string
	CredentialVersion int64
	UserKey           security.RedactedString
}

// CreateInboundCommand 创建一条专属入站；Client 必须恰好一个（domain.ValidateInboundClients）。
type CreateInboundCommand struct {
	OperationID   domain.ID
	InboundTag    string
	ListenAddress string
	Port          int
	Method        string
	Network       domain.Network
	ServerKey     security.RedactedString
	Client        InboundClient
}

type RemoveInboundCommand struct {
	OperationID domain.ID
	InboundTag  string
}

// RemoteInbound 是 Xray 中一条入站的只读观察结果。
type RemoteInbound struct {
	InboundTag string
	// PanelManaged 表示该标签位于面板保留命名空间内；false 的入站面板 MUST 只读。
	PanelManaged bool
	UserCount    int64
}

type RemoteUser struct {
	StatisticsID      string
	CredentialVersion int64
	Present           bool
	Kind              string
}

type AddUserCommand struct {
	OperationID       domain.ID
	InboundTag        string
	AllocationID      domain.ID
	StatisticsID      string
	CredentialVersion int64
	UserKey           security.RedactedString
}

type RemoveUserCommand struct {
	OperationID  domain.ID
	InboundTag   string
	StatisticsID string
}

type MutationReceipt struct {
	OperationID domain.ID
	ObservedAt  time.Time
}

type Direction string

const (
	Uplink   Direction = "uplink"
	Downlink Direction = "downlink"
)

type CounterSnapshot struct {
	StatisticsID string
	Direction    Direction
	Bytes        uint64
	ObservedAt   time.Time
	Found        bool
}

type TrafficQuery struct {
	Target        InstanceTarget
	StatisticsIDs []string
}

type TrafficRound struct {
	Observation InstanceObservation
	Snapshots   []CounterSnapshot
}

// TemplateCapabilities 是入站模板兼容性校验的结果。
//
// 说明：面板无法通过 API 读取 Xray 的 policy 段，因此「用户级统计是否开启」不可在此处证实，
// 只能在采集阶段作为健康诊断暴露（见 contracts/config.md 与 research.md R-007 更正记录）。
type TemplateCapabilities struct {
	InboundCreatable    bool
	ProtocolSupported   bool
	MethodSupported     bool
	MultiUserSupported  bool
	CompatibilityReason string
}

func (c TemplateCapabilities) Compatible() bool {
	return c.InboundCreatable && c.ProtocolSupported && c.MethodSupported && c.MultiUserSupported
}

type AdapterError struct {
	Kind        string
	Operation   string
	Retryable   bool
	SafeSummary string
}

func (e *AdapterError) Error() string {
	return fmt.Sprintf("xray %s: %s", e.Operation, e.SafeSummary)
}

const (
	ErrorInvalidArgument     = "invalid_argument"
	ErrorUnsupportedProtocol = "unsupported_protocol"
	ErrorIncompatibleProfile = "incompatible_profile"
	ErrorInstanceUnavailable = "instance_unavailable"
	ErrorDeadlineExceeded    = "deadline_exceeded"
	ErrorProfileNotFound     = "profile_not_found"
	ErrorUserAlreadyExists   = "user_already_exists"
	ErrorUserNotFound        = "user_not_found"
	// 入站生命周期的稳定错误类别（contracts/xray-adapter.md）。
	ErrorPortUnavailable      = "port_unavailable"
	ErrorInboundAlreadyExists = "inbound_already_exists"
	ErrorInboundNotFound      = "inbound_not_found"
	ErrorStatsNotFound        = "stats_not_found"
	ErrorVersionMismatch      = "version_mismatch"
	ErrorUpstreamRejected     = "upstream_rejected"
	ErrorInternal             = "internal"
)
