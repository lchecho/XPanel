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
	CreateTemplate(context.Context, TemplateRecord, domain.DomainCommand, domain.AuditEvent) (domain.ID, bool, error)
	UpdateTemplate(context.Context, TemplateRecord, RevisionMatch, domain.DomainCommand, domain.AuditEvent) error
	Template(context.Context, domain.ID) (TemplateRecord, error)
	Templates(context.Context, bool) ([]TemplateRecord, error)
	ArchiveTemplate(context.Context, domain.ID, domain.Revision, time.Time) error
	SetTemplateCompatibility(context.Context, domain.ID, domain.CompatibilityState, string, time.Time) error
	CompleteTemplateValidation(context.Context, ValidationOutcome) (bool, error)
	AdvanceCapabilityGeneration(context.Context, time.Time, bool, time.Duration, time.Time) (int64, bool, error)
	InvalidateStaleCapabilityEvidence(context.Context, int64, time.Time) ([]domain.ID, error)
	RequestRevalidation(context.Context, domain.ID, domain.Revision, domain.DomainCommand, domain.AuditEvent) (bool, error)
	// 专属入站与端口分配（data-model.md §dedicated_inbounds）
	AssignedPorts(context.Context, domain.ID) ([]int, error)
	DedicatedInbound(context.Context, domain.ID) (domain.DedicatedInbound, error)
	PanelInbounds(context.Context) ([]domain.DedicatedInbound, error)
	ConfirmInboundPresence(context.Context, domain.ID, bool, time.Time) error
	PortPoolUsage(context.Context, domain.ID) (PortPoolUsage, error)
	TemplateCounterEvidence(context.Context, domain.ID) (int, int, error)
	CreateUser(context.Context, UserCreateRecord) (domain.ID, bool, error)
	User(context.Context, domain.ID) (UserRecord, error)
	ListUsers(context.Context, UserFilter) ([]UserRecord, error)
	Enqueue(context.Context, domain.SynchronizationOperation) error
	LeaseDue(context.Context, string, time.Time, time.Duration) (*SyncWork, error)
	ConfirmIfRevisionCurrent(context.Context, domain.ID, string, domain.Revision, int64, bool, time.Time) (bool, error)
	Reschedule(context.Context, domain.ID, string, int, time.Time, string, string) error
	Supersede(context.Context, domain.ID, domain.Revision, time.Time) (int64, error)
	RecordError(context.Context, domain.ID, string, string, string, time.Time) error
	LeaseDueSync(context.Context, string, time.Time, time.Duration) (*SyncWork, error)
	ConfirmSync(context.Context, domain.ID, string, domain.Revision, int64, bool, time.Time) (bool, error)
	RenewSyncLease(context.Context, domain.ID, string, time.Time, time.Duration) (bool, error)
	RescheduleSync(context.Context, domain.ID, string, int, time.Time, string, string) error
	FailSync(context.Context, domain.ID, string, string, string, time.Time) error
	MarkInstanceHealthy(context.Context, string, time.Time) error
	MarkInstanceUnreachable(context.Context, string, string, time.Time) error
	CollectionTargets(context.Context) ([]CollectionTarget, error)
	CommitTrafficBatch(context.Context, TrafficBatch) (TrafficCommitResult, error)
	UpdateUser(context.Context, UserUpdateRecord) (bool, bool, error)
	ResetCycleTraffic(context.Context, QuotaResetRecord) (bool, bool, error)
	DueCycles(context.Context, time.Time) ([]UserRecord, error)
	NextCycleEnd(context.Context) (*time.Time, error)
	RolloverCycle(context.Context, CycleRollover) (int, bool, error)
	UpdateSettings(context.Context, string, domain.Revision, domain.DomainCommand, domain.AuditEvent) (bool, error)
	RotateCredential(context.Context, RotationRecord) (bool, error)
	ChangeInboundPort(context.Context, PortChangeRecord) (bool, error)
	SoftDeleteUser(context.Context, DeleteRecord) (bool, error)
	FailedOperations(context.Context, int) ([]FailedOperationRecord, error)
	LastCollectionAt(context.Context) (*time.Time, error)
	DailyAggregates(context.Context, domain.ID, time.Time, time.Time) ([]DailyAggregateRecord, error)
	ContinuityEvents(context.Context, domain.ID, int) ([]ContinuityEventListRecord, error)
	SyncOperations(context.Context, domain.ID, int) ([]domain.SynchronizationOperation, error)
	HasOpenOperation(context.Context, domain.ID) (bool, error)
	HasOpenRotation(context.Context, domain.ID) (bool, error)
	EnqueueReconcile(context.Context, domain.ID, domain.Revision, domain.SynchronizationOperation, time.Time) (bool, error)
	RecordObservation(context.Context, domain.ID, bool, time.Time) error
	AuditEvents(context.Context, AuditFilter) ([]domain.AuditEvent, *AuditCursor, error)
	EnqueueDriftRemoval(context.Context, domain.ID, string, string, string, time.Time) (bool, error)
	LeaseDueDriftRemoval(context.Context, string, time.Time, time.Duration) (*DriftRemoval, error)
	RenewDriftRemovalLease(context.Context, domain.ID, string, time.Time, time.Duration) (bool, error)
	StaleDriftRemovals(context.Context, domain.ID) ([]DriftRemoval, error)
	OrphanStaleDriftRemovals(context.Context) ([]DriftRemoval, error)
	CompleteDriftRemoval(context.Context, domain.ID, string, time.Time, domain.AuditEvent) error
	RescheduleDriftRemoval(context.Context, domain.ID, string, int, time.Time, string, string) error
	FailDriftRemoval(context.Context, domain.ID, string, string, string, time.Time, domain.AuditEvent) error
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
	// InsertSession / RevokeSession 让当前会话的建立与撤销和登录/登出审计处于同一写事务（T152）。
	InsertSession(context.Context, SessionRecord) error
	RevokeSession(context.Context, []byte, time.Time) (bool, error)
}

// SessionRecord 是会话存储在事务内写入的 admin_sessions 行（管理员 ID 与密码版本由事务内读取）。
type SessionRecord struct {
	ID                domain.ID
	TokenDigest       []byte
	Data              []byte
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

type writeTxKey struct{}

// ContextWithWriteTx 把正在进行的写事务放入 ctx，供会话存储的 CtxStore 路径在同一事务内写入。
func ContextWithWriteTx(ctx context.Context, tx WriteTx) context.Context {
	return context.WithValue(ctx, writeTxKey{}, tx)
}

// WriteTxFromContext 取出 ContextWithWriteTx 放入的事务。
func WriteTxFromContext(ctx context.Context) (WriteTx, bool) {
	tx, ok := ctx.Value(writeTxKey{}).(WriteTx)
	return tx, ok
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
	// CapabilityGeneration 只在确认的重启后前进；模板的兼容性证据绑定它而不是抖动的 boot epoch。
	CapabilityGeneration int64
	LastSuccessAt        *time.Time
	LastErrorCode        string
	LastErrorSummary     string
	UpdatedAt            time.Time
}

// TemplateRecord 是入站模板的持久化表示。模板不再持有服务端密钥——每条专属入站独立生成（FR-004）。
type TemplateRecord struct {
	Template domain.InboundTemplate
}

// InboundRecord 是专属入站及其加密后的服务端密钥。
type InboundRecord struct {
	Inbound              domain.DedicatedInbound
	ServerKeyCiphertext  []byte
	ServerKeyNonce       []byte
	KeyEncryptionVersion int64
}

// PortPoolUsage 供仪表盘展示端口池占用情况（FR-035）。
type PortPoolUsage struct {
	Capacity  int
	Assigned  int
	Remaining int
	Outside   []int
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
	Inbound    InboundRecord
	Credential domain.AccessCredential
	Policy     QuotaPolicyRecord
	Cycle      QuotaCycleRecord
	Operation  domain.SynchronizationOperation
	Command    domain.DomainCommand
	Audit      domain.AuditEvent
	// PortAudit 记录端口分配（FR-036）；与用户创建在同一事务内写入，摘要含端口但不含任何密钥。
	PortAudit domain.AuditEvent
}

type UserRecord struct {
	User       domain.ManagedUser
	Identity   domain.XrayUserIdentity
	Inbound    InboundRecord
	Allocation domain.AccessAllocation
	Credential domain.AccessCredential
	Template   TemplateRecord
	Policy     QuotaPolicyRecord
	Cycle      QuotaCycleRecord
}

type SyncWork struct {
	Operation  domain.SynchronizationOperation
	User       domain.ManagedUser
	Allocation domain.AccessAllocation
	Identity   domain.XrayUserIdentity
	Template   TemplateRecord
	Inbound    InboundRecord
	Credential domain.AccessCredential
}

// TrafficCursorRecord 是 traffic_cursors 表的一行。
type TrafficCursorRecord struct {
	AllocationID    domain.ID
	BootEpoch       string
	UplinkCounter   *int64
	DownlinkCounter *int64
	UplinkEpoch     int64
	DownlinkEpoch   int64
	LastObservedAt  *time.Time
	LastSuccessAt   *time.Time
	MissingSince    *time.Time
}

// CollectionTarget 是一次采集需要读取计数的分配及其当前游标、周期、策略与累计值。
type CollectionTarget struct {
	User          domain.ManagedUser
	Allocation    domain.AccessAllocation
	Identity      domain.XrayUserIdentity
	ProfileTag    string
	Policy        QuotaPolicyRecord
	Cycle         QuotaCycleRecord
	Cursor        TrafficCursorRecord
	TotalUplink   int64
	TotalDownlink int64
}

type ContinuityEventRecord struct {
	ID           domain.ID
	AllocationID domain.ID
	Event        domain.ContinuityEvent
	OccurredAt   time.Time
}

// TrafficUpdate 是采集批次中单个分配的写入内容（data-model §Atomic Transaction Boundaries 第 3 条）。
type TrafficUpdate struct {
	AllocationID  domain.ID
	Cursor        TrafficCursorRecord
	UplinkDelta   int64
	DownlinkDelta int64
	DayStartUTC   time.Time
	LocalDate     string
	Timezone      string
	Events        []ContinuityEventRecord
	// QuotaBlock 与 Audit 是模板：是否越界、operation 的 revision 与 presence 由事务内重读的事实决定。
	QuotaBlock *domain.SynchronizationOperation
	Audit      *domain.AuditEvent
}

type TrafficBatch struct {
	ObservedAt time.Time
	Updates    []TrafficUpdate
}

// UserUpdateRecord 描述编辑/启停/调额的单事务写入（data-model §Atomic Transaction Boundaries 第 2 条）。
type UserUpdateRecord struct {
	UserID           domain.ID
	ExpectedRevision domain.Revision
	DisplayName      string
	NormalizedName   string
	LimitBytes       *int64
	ResetDay         int
	AdminEnabled     bool
	// OperationTemplate 提供 ID 与创建时间；reason/phase/presence/revision 由事务内 DecideTransition 决定。
	OperationTemplate domain.SynchronizationOperation
	// QuotaAudit 是配额状态变化时写入的审计模板（quota_exceeded 或 cycle_restored 由事务内决定）。
	QuotaAudit *domain.AuditEvent
	Command    domain.DomainCommand
	Audits     []domain.AuditEvent
	Now        time.Time
}

// QuotaResetRecord 描述手动重置本周期流量（data-model §Atomic Transaction Boundaries 第 4 条）。
type QuotaResetRecord struct {
	AllocationID      domain.ID
	CycleID           domain.ID
	EventID           domain.ID
	ActorID           domain.ID
	Command           domain.DomainCommand
	OperationTemplate domain.SynchronizationOperation
	Audit             domain.AuditEvent
	Now               time.Time
}

// CycleRollover 描述周期切换（data-model §Atomic Transaction Boundaries 第 5 条）。
// CycleRollover 描述一次周期结算请求：到期的 open 周期由 Store 在同一事务内按最新 reset_day、面板时区与
// 启用/生命周期事实关闭并打开新周期（可跨多个边界），恢复操作只在事务内判定（data-model §Write Ordering）。
type CycleRollover struct {
	AllocationID      domain.ID
	OperationTemplate domain.SynchronizationOperation
	Audit             *domain.AuditEvent
	Now               time.Time
}

// TrafficCommitResult 汇总一轮采集提交创建的同步操作：越界封禁、采集提交内因样本跨越周期边界而触发的结算与恢复。
type TrafficCommitResult struct {
	Blocked  int
	Rolled   int
	Restored int
}

// RotationRecord 描述凭证轮换的单事务写入（data-model §Atomic Transaction Boundaries 第 7 条）。
type RotationRecord struct {
	UserID           domain.ID
	AllocationID     domain.ID
	ExpectedRevision domain.Revision
	Credential       domain.AccessCredential
	Operation        domain.SynchronizationOperation
	Command          domain.DomainCommand
	Audit            domain.AuditEvent
	Now              time.Time
}

// PortChangeRecord 描述「更换专属端口」的单事务写入：校验池范围与唯一性、改端口、写同步意图与审计，
// 全部在一个事务内完成；revision 与请求指纹保证并发或重复提交不产生部分状态（FR-010）。
type PortChangeRecord struct {
	UserID           domain.ID
	AllocationID     domain.ID
	ExpectedRevision domain.Revision
	OldPort          int
	NewPort          int
	Operation        domain.SynchronizationOperation
	Command          domain.DomainCommand
	Audit            domain.AuditEvent
	Now              time.Time
}

// DeleteRecord 描述软删除的单事务写入（data-model §Atomic Transaction Boundaries 第 8 条）。
type DeleteRecord struct {
	UserID           domain.ID
	AllocationID     domain.ID
	ExpectedRevision domain.Revision
	Operation        domain.SynchronizationOperation
	Command          domain.DomainCommand
	Audit            domain.AuditEvent
	Now              time.Time
}

// FailedOperationRecord 是仪表盘“最近同步失败”摘要的一行。
type FailedOperationRecord struct {
	UserID        domain.ID
	DisplayName   string
	Reason        domain.SyncReason
	State         domain.SyncState
	ErrorCode     string
	ErrorSummary  string
	AttemptCount  int
	NextAttemptAt time.Time
	UpdatedAt     time.Time
}

// DailyAggregateRecord 是 daily_traffic_aggregates 的一行。
type DailyAggregateRecord struct {
	DayStartUTC   time.Time
	LocalDate     string
	Timezone      string
	UplinkBytes   int64
	DownlinkBytes int64
}

// ContinuityEventListRecord 是详情页展示的连续性事件。
type ContinuityEventListRecord struct {
	Type       string
	Direction  string
	OccurredAt time.Time
	Summary    string
}

// AuditCursor 是审计列表的游标（按 occurred_at, id 倒序）。
type AuditCursor struct {
	OccurredAt time.Time
	ID         domain.ID
}

// AuditFilter 描述审计页筛选；空值表示不限制。
type AuditFilter struct {
	TargetID domain.ID
	Action   string
	Result   string
	Before   *AuditCursor
	Limit    int
}

// DriftRemoval 是持久化的“移除面板命名空间内未知身份”意图（data-model §DriftRemoval，迁移 00002）。
type DriftRemoval struct {
	ID         domain.ID
	TemplateID domain.ID
	InboundTag string
	// Kind 区分两类漂移目标：identity 为面板入站内的未知客户端，inbound 为面板命名空间内的孤立入站。
	Kind          string
	StatisticsID  string
	State         domain.SyncState
	AttemptCount  int
	NextAttemptAt time.Time
	// ExpectedStatisticsID 是该入站归属用户的统计身份，领取时按入站标签查出（孤立入站为空）。
	// 它让 RemoveUser 的最后客户端守卫无需从标签推断归属（T097）。
	ExpectedStatisticsID string
	// Reclaimed 表示本次领取回收了另一个 worker 的过期租约：其 RemoveUser 可能已生效，执行前必须先读实际身份。
	Reclaimed bool
}

// ValidationOutcome 是一次 profile 验证的完整结果，按 ExpectedRevision 条件在一个事务中提交（FR-021）。
type ValidationOutcome struct {
	TemplateID       domain.ID
	ExpectedRevision domain.Revision
	State            domain.CompatibilityState
	Reason           string
	ValidatedAt      time.Time
	Health           string
	BootEpoch        string
	// CapabilityGeneration 是做出本次结论时的能力世代；只有 compatible 结论才会落库。
	CapabilityGeneration int64
	ErrorCode            string
	ErrorSummary         string
	SuccessAt            *time.Time
	Audit                domain.AuditEvent
}
