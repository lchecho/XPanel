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
	// Network 必须与模板一致：探针要按模板实际会用的网络能力发送流量，
	// 否则「tcp_udp 模板只验证了 TCP」这种缺口不会被发现（FR-005）。
	Network domain.Network
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

// RemoveUserCommand 移除某条专属入站内的一个客户端。
//
// ExpectedStatisticsID 是该入站**归属用户**的统计身份，必须由调用方显式给出：适配器据此判断
// 「移除之后这条入站是否还留有它真正的受管客户端」。它不能由入站标签推断——那是把两个各自演进的
// 命名规则绑死成隐式契约，一旦其中之一变化，守卫就会在没有任何测试报警的情况下失效（T097）。
type RemoveUserCommand struct {
	OperationID          domain.ID
	InboundTag           string
	StatisticsID         string
	ExpectedStatisticsID string
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
// 四项硬性能力：能创建入站、能移除入站、支持 SS2022 多用户身份、用户级流量统计可读。
// 最后一项无法通过读配置证实（面板读不到 Xray 的 policy 段），只能在一次性探针入站上
// 产生经过身份认证的最小流量后回读计数器（research.md C-007）。
type TemplateCapabilities struct {
	InboundCreatable bool
	// InboundRemovable 表示探针入站确实被移除了。面板的整个生命周期依赖「能建也能拆」，
	// 只能建不能拆的节点会让停用、删除与配额封禁全部无法生效，必须判为不兼容（FR-005）。
	InboundRemovable   bool
	ProtocolSupported  bool
	MethodSupported    bool
	MultiUserSupported bool
	// TrafficAccounted 表示探针身份的上行与下行计数器都读到了：节点确实开启了用户级统计。
	// 这是 FR-005 的第四项能力，只能通过在探针入站上产生真实流量来证实（research.md C-007）。
	TrafficAccounted bool
	// ProbeStatisticsID 是本次验证使用的一次性探针身份，供测试与诊断核对清理结果；
	// 它从不属于任何用户，也不进入任何计量口径。
	ProbeStatisticsID   string
	CompatibilityReason string
}

func (c TemplateCapabilities) Compatible() bool {
	return c.InboundCreatable && c.InboundRemovable && c.ProtocolSupported && c.MethodSupported &&
		c.MultiUserSupported && c.TrafficAccounted
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
	// ErrorLastManagedClient：该移除会让入站失去最后一个受管客户端，被适配器在发起 RPC 前拒绝。
	ErrorLastManagedClient = "last_managed_client"
	ErrorStatsNotFound     = "stats_not_found"
	ErrorVersionMismatch   = "version_mismatch"
	ErrorUpstreamRejected  = "upstream_rejected"
	ErrorInternal          = "internal"
)
