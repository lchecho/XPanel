package domain

import (
	"errors"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"xpanel/internal/security"
)

type CompatibilityState string

const (
	CompatibilityUnverified   CompatibilityState = "unverified"
	CompatibilityCompatible   CompatibilityState = "compatible"
	CompatibilityIncompatible CompatibilityState = "incompatible"
	CompatibilityUnreachable  CompatibilityState = "unreachable"
)

type Network string

const (
	NetworkTCP    Network = "tcp"
	NetworkUDP    Network = "udp"
	NetworkTCPUDP Network = "tcp_udp"
)

// 数据模型：InboundTemplate 描述面板应当如何为用户创建专属入站（spec 术语「入站模板」）。
//
// 职责：承载公开主机名、监听地址、端口池、加密方式与网络能力；它本身不是一个可连接的入口。
// 约束：不再持有服务端密钥与 bootstrap 身份——每条专属入站的服务端密钥由面板独立生成（FR-004）。
//
// 字段：
//
//	PublicHost：交付给使用者的主机名，必填
//	ListenAddress：入站监听地址，必填且必须是 IP 字面量
//	Pool：可分配端口闭区间，面板只在池内分配
//	Method：仅支持 SS2022 AES-128/256-GCM
type InboundTemplate struct {
	ID                  ID
	InstanceID          ID
	Name                string
	NormalizedName      string
	PublicHost          string
	ListenAddress       string
	Pool                PortPool
	Method              string
	Network             Network
	Compatibility       CompatibilityState
	CompatibilityReason string
	LastValidatedAt     *time.Time
	// ValidatedGeneration 记录通过能力门禁时的 Xray 能力世代。世代只在**确认的**重启后前进，
	// 因此不会被 boot epoch 的一秒量化抖动误伤；与当前世代不符即视为证据过期（T094）。
	ValidatedGeneration int64
	Revision            Revision
	ArchivedAt          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func NewInboundTemplate(id, instanceID ID, name, publicHost, listenAddress string, poolStart, poolEnd int,
	method string, network Network, now time.Time) (InboundTemplate, error) {
	normalized, err := NormalizeDisplayName(name)
	if err != nil {
		return InboundTemplate{}, &ValidationError{Field: "name", Message: err.Error()}
	}
	if strings.TrimSpace(publicHost) == "" {
		return InboundTemplate{}, &ValidationError{Field: "public_host", Message: "public host is required"}
	}
	if ip := net.ParseIP(strings.Trim(publicHost, "[]")); ip == nil && strings.ContainsAny(publicHost, " /?#") {
		return InboundTemplate{}, &ValidationError{Field: "public_host", Message: "public host is invalid"}
	}
	listen := strings.TrimSpace(listenAddress)
	if net.ParseIP(listen) == nil {
		return InboundTemplate{}, &ValidationError{Field: "listen_address", Message: "listen address must be an IP literal"}
	}
	pool, err := NewPortPool(poolStart, poolEnd)
	if err != nil {
		return InboundTemplate{}, err
	}
	if method != security.MethodAES128 && method != security.MethodAES256 {
		return InboundTemplate{}, &ValidationError{Field: "method", Message: "unsupported Shadowsocks 2022 method"}
	}
	if network != NetworkTCP && network != NetworkUDP && network != NetworkTCPUDP {
		return InboundTemplate{}, &ValidationError{Field: "network", Message: "network must be tcp, udp, or tcp_udp"}
	}
	return InboundTemplate{ID: id, InstanceID: instanceID, Name: strings.TrimSpace(name), NormalizedName: normalized,
		PublicHost: strings.TrimSpace(publicHost), ListenAddress: listen, Pool: pool, Method: method, Network: network,
		Compatibility: CompatibilityUnverified, Revision: 0, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

func NormalizeDisplayName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if count := utf8.RuneCountInString(trimmed); count < 1 || count > 64 {
		return "", errors.New("display name must contain 1 to 64 characters")
	}
	if strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return "", errors.New("display name must not contain control characters")
	}
	return strings.ToLower(strings.Join(strings.Fields(norm.NFKC.String(trimmed)), " ")), nil
}

func (p *InboundTemplate) ApplyCompatibility(state CompatibilityState, reason string, now time.Time) error {
	if state != CompatibilityCompatible && state != CompatibilityIncompatible && state != CompatibilityUnreachable && state != CompatibilityUnverified {
		return &InvalidStateError{Message: "unknown profile compatibility state"}
	}
	p.Compatibility = state
	p.CompatibilityReason = reason
	value := now.UTC()
	p.LastValidatedAt = &value
	p.UpdatedAt = value
	return nil
}

type IdentityKind string

const (
	IdentityManaged IdentityKind = "managed"
)

type XrayUserIdentity struct {
	ID           ID
	InstanceID   ID
	TemplateID   ID
	StatisticsID string
	Kind         IdentityKind
	CreatedAt    time.Time
}
