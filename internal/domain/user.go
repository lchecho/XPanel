package domain

import (
	"strings"
	"time"
)

type LifecycleState string

const (
	LifecycleActive  LifecycleState = "active"
	LifecycleDeleted LifecycleState = "deleted"
)

type QuotaState string

const (
	QuotaWithinLimit QuotaState = "within_limit"
	QuotaExceeded    QuotaState = "exceeded"
)

type ProjectionState string

const (
	ProjectionUnknown ProjectionState = "unknown"
	ProjectionPending ProjectionState = "pending"
	ProjectionPresent ProjectionState = "present"
	ProjectionAbsent  ProjectionState = "absent"
	ProjectionError   ProjectionState = "error"
)

type ManagedUser struct {
	ID             ID
	DisplayName    string
	NormalizedName string
	Lifecycle      LifecycleState
	Revision       Revision
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

func NewManagedUser(id ID, displayName string, now time.Time) (ManagedUser, error) {
	normalized, err := NormalizeDisplayName(displayName)
	if err != nil {
		return ManagedUser{}, &ValidationError{Field: "display_name", Message: err.Error()}
	}
	return ManagedUser{ID: id, DisplayName: strings.TrimSpace(displayName), NormalizedName: normalized, Lifecycle: LifecycleActive,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

type AccessAllocation struct {
	ID                       ID
	UserID                   ID
	TemplateID               ID
	IdentityID               ID
	AdminEnabled             bool
	QuotaState               QuotaState
	ProjectionState          ProjectionState
	ObservedPresent          *bool
	DesiredRevision          Revision
	SyncedRevision           Revision
	DesiredCredentialVersion int64
	SyncedCredentialVersion  *int64
	LastSyncAt               *time.Time
	LastSyncErrorCode        string
	LastSyncErrorSummary     string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func (a AccessAllocation) DesiredPresent(user ManagedUser) bool {
	return user.Lifecycle == LifecycleActive && a.AdminEnabled && a.QuotaState == QuotaWithinLimit
}

type DisplayState string

const (
	DisplayDeleted        DisplayState = "deleted"
	DisplayDisabling      DisplayState = "disabling"
	DisplayDisabled       DisplayState = "disabled"
	DisplayQuotaDisabling DisplayState = "quota_disabling"
	DisplayQuotaExceeded  DisplayState = "quota_exceeded"
	DisplayEnabling       DisplayState = "enabling"
	DisplayActive         DisplayState = "active"
)

func (a AccessAllocation) DisplayState(user ManagedUser) DisplayState {
	if user.Lifecycle == LifecycleDeleted {
		return DisplayDeleted
	}
	if !a.AdminEnabled && (a.ProjectionState == ProjectionPresent || a.ProjectionState == ProjectionPending) {
		return DisplayDisabling
	}
	if !a.AdminEnabled {
		return DisplayDisabled
	}
	if a.QuotaState == QuotaExceeded && (a.ProjectionState == ProjectionPresent || a.ProjectionState == ProjectionPending) {
		return DisplayQuotaDisabling
	}
	if a.QuotaState == QuotaExceeded {
		return DisplayQuotaExceeded
	}
	if a.ProjectionState != ProjectionPresent {
		return DisplayEnabling
	}
	return DisplayActive
}

func (a AccessAllocation) PendingSync() bool {
	return a.DesiredRevision != a.SyncedRevision || a.ProjectionState == ProjectionUnknown ||
		a.ProjectionState == ProjectionPending || a.ProjectionState == ProjectionError
}

type CredentialState string

const (
	CredentialPending   CredentialState = "pending"
	CredentialActive    CredentialState = "active"
	CredentialRetired   CredentialState = "retired"
	CredentialDestroyed CredentialState = "destroyed"
)

type AccessCredential struct {
	ID                   ID
	AllocationID         ID
	Version              int64
	State                CredentialState
	KeyCiphertext        []byte
	KeyNonce             []byte
	KeyEncryptionVersion int64
	CreatedAt            time.Time
	ActivatedAt          *time.Time
	RetiredAt            *time.Time
}

func ValidateCredentialSet(credentials []AccessCredential) error {
	pending, active := 0, 0
	for _, credential := range credentials {
		if credential.Version < 1 {
			return &InvalidStateError{Message: "credential version must be positive"}
		}
		if credential.State == CredentialDestroyed {
			if len(credential.KeyCiphertext) != 0 || len(credential.KeyNonce) != 0 {
				return &InvalidStateError{Message: "destroyed credential retains key material"}
			}
		} else if len(credential.KeyCiphertext) == 0 || len(credential.KeyNonce) == 0 {
			return &InvalidStateError{Message: "usable credential lacks key material"}
		}
		switch credential.State {
		case CredentialPending:
			pending++
		case CredentialActive:
			active++
		}
	}
	if pending > 1 || active > 1 {
		return &InvalidStateError{Message: "allocation has multiple pending or active credentials"}
	}
	return nil
}
