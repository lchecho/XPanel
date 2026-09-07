package domain

import (
	"fmt"
	"time"
)

type SyncReason string

const (
	SyncCreate       SyncReason = "create"
	SyncEnable       SyncReason = "enable"
	SyncDisable      SyncReason = "disable"
	SyncQuotaBlock   SyncReason = "quota_block"
	SyncQuotaRestore SyncReason = "quota_restore"
	SyncRotate       SyncReason = "rotate"
	SyncDelete       SyncReason = "delete"
	SyncReconcile    SyncReason = "reconcile"
)

type SyncPhase string

const (
	SyncCreateInbound SyncPhase = "create_inbound"
	SyncRemoveInbound SyncPhase = "remove_inbound"
	SyncRemoveOld     SyncPhase = "remove_old"
	SyncAddDesired    SyncPhase = "add_desired"
	SyncConfirm       SyncPhase = "confirm"
	SyncDone          SyncPhase = "done"
)

type SyncState string

const (
	SyncPending         SyncState = "pending"
	SyncLeased          SyncState = "leased"
	SyncRetryWait       SyncState = "retry_wait"
	SyncSucceeded       SyncState = "succeeded"
	SyncSuperseded      SyncState = "superseded"
	SyncPermanentFailed SyncState = "permanent_failed"
)

type SynchronizationOperation struct {
	ID                       ID
	AllocationID             ID
	DesiredRevision          Revision
	DesiredPresence          bool
	DesiredCredentialVersion *int64
	Reason                   SyncReason
	Phase                    SyncPhase
	State                    SyncState
	IdempotencyKey           string
	AttemptCount             int
	NextAttemptAt            time.Time
	LeaseOwner               string
	LeaseExpiresAt           *time.Time
	LastErrorCode            string
	LastErrorSummary         string
	CreatedAt                time.Time
	StartedAt                *time.Time
	CompletedAt              *time.Time
}

// InboundPhaseFor 由期望监听状态给出专属入站的同步阶段。
//
// 职责：阶段名是持久化的意图标签，决定同步失败后从哪一步恢复，也是审计与故障矩阵区分
// 「创建入站」与「移除入站」的依据；凭证轮换不经过它（轮换在既有入站内两阶段完成）。
func InboundPhaseFor(present bool) SyncPhase {
	if present {
		return SyncCreateInbound
	}
	return SyncRemoveInbound
}

func SyncIdempotencyKey(allocationID ID, revision Revision, phase SyncPhase) string {
	return fmt.Sprintf("%s:%d:%s", allocationID, revision, phase)
}

func NewSynchronizationOperation(id, allocationID ID, revision Revision, present bool, credentialVersion *int64,
	reason SyncReason, phase SyncPhase, now time.Time) SynchronizationOperation {
	return SynchronizationOperation{ID: id, AllocationID: allocationID, DesiredRevision: revision,
		DesiredPresence: present, DesiredCredentialVersion: credentialVersion, Reason: reason, Phase: phase,
		State: SyncPending, IdempotencyKey: SyncIdempotencyKey(allocationID, revision, phase),
		NextAttemptAt: now.UTC(), CreatedAt: now.UTC()}
}

func (o *SynchronizationOperation) Supersede(current Revision, now time.Time) bool {
	if o.DesiredRevision >= current || o.State == SyncSucceeded || o.State == SyncSuperseded {
		return false
	}
	o.State = SyncSuperseded
	value := now.UTC()
	o.CompletedAt = &value
	o.LeaseOwner = ""
	o.LeaseExpiresAt = nil
	return true
}

func NextBackoff(attempt int, max time.Duration, random func(int64) int64) time.Duration {
	if max <= 0 {
		max = 30 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}
	capDuration := time.Second
	for i := 0; i < attempt && capDuration < max; i++ {
		if capDuration > max/2 {
			capDuration = max
			break
		}
		capDuration *= 2
	}
	if capDuration > max {
		capDuration = max
	}
	if random == nil {
		return capDuration
	}
	value := random(int64(capDuration) + 1)
	if value < 0 {
		value = -value
	}
	return time.Duration(value % (int64(capDuration) + 1))
}
