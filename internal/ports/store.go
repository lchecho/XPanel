package ports

import (
	"context"
	"time"

	"xpanel/internal/domain"
)

type AuthStore interface {
	WithWriteTx(context.Context, func(WriteTx) error) error
	WithReadTx(context.Context, func(ReadTx) error) error
}

type Store interface {
	AuthStore
	ManagedInstance(context.Context) (ManagedInstanceRecord, error)
	Settings(context.Context) (PanelSettingsRecord, error)
	SetInstanceHealth(context.Context, string, string, string, *time.Time, string, time.Time) error
	AppendAudit(context.Context, domain.AuditEvent) error
	CreateProfile(context.Context, ProfileRecord, domain.DomainCommand, domain.AuditEvent) (domain.ID, bool, error)
	UpdateProfile(context.Context, ProfileRecord, RevisionMatch, domain.DomainCommand, domain.AuditEvent) error
	Profile(context.Context, domain.ID) (ProfileRecord, error)
	Profiles(context.Context, bool) ([]ProfileRecord, error)
	ArchiveProfile(context.Context, domain.ID, domain.Revision, time.Time) error
	SetProfileCompatibility(context.Context, domain.ID, domain.CompatibilityState, string, time.Time) error
	RegisterBootstrapIdentity(context.Context, domain.XrayUserIdentity) error
	CreateUser(context.Context, UserCreateRecord) (domain.ID, bool, error)
	User(context.Context, domain.ID) (UserRecord, error)
	ListUsers(context.Context, UserFilter) ([]UserRecord, error)
	Enqueue(context.Context, domain.SynchronizationOperation) error
	LeaseDue(context.Context, string, time.Time, time.Duration) (*SyncWork, error)
	ConfirmIfRevisionCurrent(context.Context, domain.ID, domain.Revision, int64, bool, time.Time) (bool, error)
	Reschedule(context.Context, domain.ID, int, time.Time, string, string) error
	Supersede(context.Context, domain.ID, domain.Revision, time.Time) (int64, error)
	RecordError(context.Context, domain.ID, string, string, time.Time) error
	LeaseDueSync(context.Context, string, time.Time, time.Duration) (*SyncWork, error)
	ConfirmSync(context.Context, domain.ID, domain.Revision, int64, bool, time.Time) (bool, error)
	RescheduleSync(context.Context, domain.ID, int, time.Time, string, string) error
	FailSync(context.Context, domain.ID, string, string, time.Time) error
	AdvancePhase(context.Context, domain.ID, domain.SyncPhase, time.Time) error
	MarkInstanceHealthy(context.Context, string, time.Time) error
	MarkInstanceUnreachable(context.Context, string, string, time.Time) error
	Close() error
}

type ReadTx interface {
	FindCommand(context.Context, domain.ID) (*domain.DomainCommand, error)
	Administrator(context.Context) (*AdministratorRecord, error)
	PanelSettings(context.Context) (PanelSettingsRecord, error)
}

type WriteTx interface {
	ReadTx
	SaveCommand(context.Context, domain.DomainCommand) error
	AppendAudit(context.Context, domain.AuditEvent) error
	CreateAdministrator(context.Context, AdministratorRecord) error
	UpdateAdministratorPassword(context.Context, domain.ID, string, int64, time.Time) error
	RevokeAllSessions(context.Context, domain.ID, time.Time) error
}

type AdministratorRecord struct {
	ID              domain.ID
	Username        string
	PasswordHash    string
	PasswordVersion int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type PanelSettingsRecord struct {
	QuotaTimezone        string
	KeyVerifier          []byte
	KeyVerifierNonce     []byte
	KeyEncryptionVersion int64
	Revision             int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type ManagedInstanceRecord struct {
	ID                      domain.ID
	Name                    string
	APIEndpoint             string
	SupportedRuntimeVersion string
	HealthState             string
	BootEpoch               string
	LastSuccessAt           *time.Time
	LastErrorCode           string
	LastErrorSummary        string
	UpdatedAt               time.Time
}

type ProfileRecord struct {
	Profile              domain.AccessProfile
	ServerKeyCiphertext  []byte
	ServerKeyNonce       []byte
	KeyEncryptionVersion int64
}

type RevisionMatch struct{ Expected domain.Revision }

// UserFilter 描述用户列表的搜索与筛选条件；Status 取值见 domain.DisplayState 与 "pending"。
type UserFilter struct {
	Query          string
	Status         string
	IncludeDeleted bool
}

type QuotaPolicyRecord struct {
	AllocationID domain.ID
	LimitBytes   *int64
	ResetDay     int
	Revision     domain.Revision
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type QuotaCycleRecord struct {
	ID                     domain.ID
	AllocationID           domain.ID
	StartsAt               time.Time
	EndsAt                 time.Time
	Timezone               string
	ResetDay               int
	Status                 string
	GrossUplinkBytes       int64
	GrossDownlinkBytes     int64
	AccountedUplinkBytes   int64
	AccountedDownlinkBytes int64
	ManualResetCount       int64
	OpenedAt               time.Time
	ClosedAt               *time.Time
}

type UserCreateRecord struct {
	User       domain.ManagedUser
	Identity   domain.XrayUserIdentity
	Allocation domain.AccessAllocation
	Credential domain.AccessCredential
	Policy     QuotaPolicyRecord
	Cycle      QuotaCycleRecord
	Operation  domain.SynchronizationOperation
	Command    domain.DomainCommand
	Audit      domain.AuditEvent
}

type UserRecord struct {
	User       domain.ManagedUser
	Identity   domain.XrayUserIdentity
	Allocation domain.AccessAllocation
	Credential domain.AccessCredential
	Profile    ProfileRecord
	Policy     QuotaPolicyRecord
	Cycle      QuotaCycleRecord
}

type SyncWork struct {
	Operation  domain.SynchronizationOperation
	User       domain.ManagedUser
	Allocation domain.AccessAllocation
	Identity   domain.XrayUserIdentity
	Profile    ProfileRecord
	Credential domain.AccessCredential
}
