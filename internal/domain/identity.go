package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type ID string
type Revision int64

func NewID() (ID, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("generate identifier")
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return ID(fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(raw[0:4]), hex.EncodeToString(raw[4:6]),
		hex.EncodeToString(raw[6:8]), hex.EncodeToString(raw[8:10]), hex.EncodeToString(raw[10:16]))), nil
}

func (id ID) String() string { return string(id) }
func (id ID) Valid() bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

type ActorType string

const (
	ActorAdministrator ActorType = "administrator"
	ActorLocalCLI      ActorType = "local_cli"
	ActorSystem        ActorType = "system"
)

type CommandState string

const (
	CommandAccepted  CommandState = "accepted"
	CommandCompleted CommandState = "completed"
	CommandFailed    CommandState = "failed"
)

type DomainCommand struct {
	ID                 ID
	ActorType          ActorType
	ActorID            *ID
	CommandType        string
	TargetType         string
	TargetID           ID
	RequestFingerprint []byte
	State              CommandState
	ResultReference    string
	CreatedAt          time.Time
	CompletedAt        *time.Time
}

func Fingerprint(parts ...string) []byte {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return h.Sum(nil)
}

type AuditResult string

const (
	AuditAccepted   AuditResult = "accepted"
	AuditSucceeded  AuditResult = "succeeded"
	AuditFailed     AuditResult = "failed"
	AuditSuperseded AuditResult = "superseded"
)

const (
	ActionLogin                         = "login"
	ActionLogout                        = "logout"
	ActionAdministratorInitialized      = "administrator_initialized"
	ActionPasswordReset                 = "password_reset"
	ActionUserCreated                   = "user_created"
	ActionUserUpdated                   = "user_updated"
	ActionUserEnabled                   = "user_enabled"
	ActionUserDisabled                  = "user_disabled"
	ActionCredentialRotated             = "credential_rotated"
	ActionUserDeleted                   = "user_deleted"
	ActionQuotaExceeded                 = "quota_exceeded"
	ActionTrafficReset                  = "traffic_reset"
	ActionCycleRestored                 = "cycle_restored"
	ActionSyncFailed                    = "sync_failed"
	ActionSyncSucceeded                 = "sync_succeeded"
	ActionTemplateRegistered            = "template_registered"
	ActionTemplateUpdated               = "template_updated"
	ActionTemplateValidated             = "template_validated"
	ActionTemplateRevalidationRequested = "template_revalidation_requested"
	ActionInboundCreated                = "inbound_created"
	ActionInboundRemoved                = "inbound_removed"
	ActionPortAssigned                  = "port_assigned"
	ActionPortReleased                  = "port_released"
	ActionPortChanged                   = "port_changed"
	ActionSettingsUpdated               = "settings_updated"
	ActionReconcileRemovedUnknown       = "reconcile_removed_unknown"
)

type AuditEvent struct {
	ID          ID
	OccurredAt  time.Time
	ActorType   ActorType
	ActorID     *ID
	TargetType  string
	TargetID    ID
	Action      string
	Result      AuditResult
	CommandID   *ID
	OperationID *ID
	SafeSummary string
}
