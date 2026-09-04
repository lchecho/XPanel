package ports

import (
	"context"
	"fmt"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/security"
)

type Adapter interface {
	Probe(context.Context, InstanceTarget) (InstanceObservation, error)
	ValidateProfile(context.Context, RuntimeProfile) (ProfileCapabilities, error)
	ListUsers(context.Context, RuntimeProfile) ([]RemoteUser, error)
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

type RuntimeProfile struct {
	ID                    domain.ID
	InboundTag            string
	Method                string
	BootstrapStatisticsID string
	ServiceKey            security.RedactedString
}

type RemoteUser struct {
	StatisticsID      string
	CredentialVersion int64
	Present           bool
	Kind              string
}

type AddUserCommand struct {
	OperationID       domain.ID
	ProfileTag        string
	AllocationID      domain.ID
	StatisticsID      string
	CredentialVersion int64
	UserKey           security.RedactedString
}

type RemoveUserCommand struct {
	OperationID  domain.ID
	ProfileTag   string
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

type ProfileCapabilities struct {
	InboundPresent      bool
	ProtocolSupported   bool
	MethodSupported     bool
	MultiUserSupported  bool
	IndependentStats    bool
	BootstrapVisible    bool
	CompatibilityReason string
}

func (c ProfileCapabilities) Compatible() bool {
	return c.InboundPresent && c.ProtocolSupported && c.MethodSupported && c.MultiUserSupported && c.IndependentStats && c.BootstrapVisible
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
	ErrorStatsNotFound       = "stats_not_found"
	ErrorVersionMismatch     = "version_mismatch"
	ErrorUpstreamRejected    = "upstream_rejected"
	ErrorInternal            = "internal"
)
