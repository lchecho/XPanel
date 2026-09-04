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

type AccessProfile struct {
	ID                    ID
	InstanceID            ID
	Name                  string
	NormalizedName        string
	InboundTag            string
	PublicHost            string
	PublicPort            int
	Method                string
	Network               Network
	BootstrapStatisticsID string
	Compatibility         CompatibilityState
	CompatibilityReason   string
	LastValidatedAt       *time.Time
	Revision              Revision
	ArchivedAt            *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func NewAccessProfile(id, instanceID ID, name, inboundTag, publicHost string, publicPort int, method string,
	network Network, bootstrapID string, now time.Time) (AccessProfile, error) {
	normalized, err := NormalizeDisplayName(name)
	if err != nil {
		return AccessProfile{}, &ValidationError{Field: "name", Message: err.Error()}
	}
	if strings.TrimSpace(inboundTag) == "" || strings.IndexFunc(inboundTag, unicode.IsControl) >= 0 {
		return AccessProfile{}, &ValidationError{Field: "inbound_tag", Message: "inbound tag must be non-empty and contain no control characters"}
	}
	if strings.TrimSpace(publicHost) == "" {
		return AccessProfile{}, &ValidationError{Field: "public_host", Message: "public host is required"}
	}
	if ip := net.ParseIP(strings.Trim(publicHost, "[]")); ip == nil && strings.ContainsAny(publicHost, " /?#") {
		return AccessProfile{}, &ValidationError{Field: "public_host", Message: "public host is invalid"}
	}
	if publicPort < 1 || publicPort > 65535 {
		return AccessProfile{}, &ValidationError{Field: "public_port", Message: "public port must be between 1 and 65535"}
	}
	if method != security.MethodAES128 && method != security.MethodAES256 {
		return AccessProfile{}, &ValidationError{Field: "method", Message: "unsupported Shadowsocks 2022 method"}
	}
	if network != NetworkTCP && network != NetworkUDP && network != NetworkTCPUDP {
		return AccessProfile{}, &ValidationError{Field: "network", Message: "network must be tcp, udp, or tcp_udp"}
	}
	if strings.TrimSpace(bootstrapID) == "" || strings.Contains(bootstrapID, ">>>") || strings.HasPrefix(bootstrapID, "xpanel-") {
		return AccessProfile{}, &ValidationError{Field: "bootstrap_statistics_id", Message: "bootstrap identity is invalid or reserved"}
	}
	return AccessProfile{ID: id, InstanceID: instanceID, Name: strings.TrimSpace(name), NormalizedName: normalized,
		InboundTag: strings.TrimSpace(inboundTag), PublicHost: strings.TrimSpace(publicHost), PublicPort: publicPort,
		Method: method, Network: network, BootstrapStatisticsID: bootstrapID, Compatibility: CompatibilityUnverified,
		Revision: 0, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
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

func (p *AccessProfile) ApplyCompatibility(state CompatibilityState, reason string, now time.Time) error {
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
	IdentityBootstrap IdentityKind = "bootstrap"
	IdentityManaged   IdentityKind = "managed"
)

type XrayUserIdentity struct {
	ID           ID
	InstanceID   ID
	ProfileID    ID
	StatisticsID string
	Kind         IdentityKind
	CreatedAt    time.Time
}
