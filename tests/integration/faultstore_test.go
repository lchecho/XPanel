package integration

import (
	"context"
	"errors"
	"sync"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
)

var errInjected = errors.New("injected database commit failure")

// faultStore 是可注入一次性提交失败的 Store 包装器，用于故障矩阵中的“数据库提交失败”列。
type faultStore struct {
	ports.Store
	mu    sync.Mutex
	fails map[string]bool
}

func newFaultStore(inner ports.Store) *faultStore {
	return &faultStore{Store: inner, fails: map[string]bool{}}
}

// FailNext 让下一次指定写入返回错误而不执行。
func (f *faultStore) FailNext(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[method] = true
}

func (f *faultStore) take(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails[method] {
		delete(f.fails, method)
		return true
	}
	return false
}

func (f *faultStore) CreateUser(ctx context.Context, record ports.UserCreateRecord) (domain.ID, bool, error) {
	if f.take("CreateUser") {
		return "", false, errInjected
	}
	return f.Store.CreateUser(ctx, record)
}

func (f *faultStore) UpdateUser(ctx context.Context, record ports.UserUpdateRecord) (bool, bool, error) {
	if f.take("UpdateUser") {
		return false, false, errInjected
	}
	return f.Store.UpdateUser(ctx, record)
}

func (f *faultStore) RotateCredential(ctx context.Context, record ports.RotationRecord) (bool, error) {
	if f.take("RotateCredential") {
		return false, errInjected
	}
	return f.Store.RotateCredential(ctx, record)
}

func (f *faultStore) ChangeInboundPort(ctx context.Context, record ports.PortChangeRecord) (bool, error) {
	if f.take("ChangeInboundPort") {
		return false, errInjected
	}
	return f.Store.ChangeInboundPort(ctx, record)
}

func (f *faultStore) SoftDeleteUser(ctx context.Context, record ports.DeleteRecord) (bool, error) {
	if f.take("SoftDeleteUser") {
		return false, errInjected
	}
	return f.Store.SoftDeleteUser(ctx, record)
}

func (f *faultStore) CommitTrafficBatch(ctx context.Context, batch ports.TrafficBatch) (ports.TrafficCommitResult, error) {
	if f.take("CommitTrafficBatch") {
		return ports.TrafficCommitResult{}, errInjected
	}
	return f.Store.CommitTrafficBatch(ctx, batch)
}

func (f *faultStore) RolloverCycle(ctx context.Context, rollover ports.CycleRollover) (int, bool, error) {
	if f.take("RolloverCycle") {
		return 0, false, errInjected
	}
	return f.Store.RolloverCycle(ctx, rollover)
}

func (f *faultStore) ConfirmSync(ctx context.Context, id domain.ID, owner string, revision domain.Revision, version int64, present bool, now time.Time) (bool, error) {
	if f.take("ConfirmSync") {
		return false, errInjected
	}
	return f.Store.ConfirmSync(ctx, id, owner, revision, version, present, now)
}
