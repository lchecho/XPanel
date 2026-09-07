package integration

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
	"xpanel/internal/worker"
)

// 备份（VACUUM INTO）→ 在新路径恢复 → 主密钥校验 → 对重启后的 Xray 协调：应启用用户恢复，封禁/删除用户不复活。
func TestBackupRestoreAndReconcileAfterRestore(t *testing.T) {
	app := testsupport.New(t)
	profileID := app.RegisterCompatibleProfile("Primary")
	limit := int64(1 << 20)
	active := app.CreateUser("Active", profileID, nil)
	blocked := app.CreateUser("Blocked", profileID, &limit)
	deleted := app.CreateUser("Deleted", profileID, nil)
	app.Drain()
	app.SetTraffic(blocked, 1<<20, 0)
	app.Collect()
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: deleted.User.ID, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()

	backup := filepath.Join(t.TempDir(), "xpanel-backup.db")
	if _, err := app.Store.DB().Write.ExecContext(context.Background(), `VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := sqlite.Open(context.Background(), backup, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := sqlite.Migrate(context.Background(), restored.Write); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(restored)
	wrongKeyring, _ := security.NewKeyring(bytes.Repeat([]byte{1}, 32))
	if err := store.VerifyKey(context.Background(), wrongKeyring.Verify); err == nil {
		t.Fatal("restored database accepted a mismatched root key")
	}
	if err := store.VerifyKey(context.Background(), app.Keyring.Verify); err != nil {
		t.Fatalf("restored database rejected the original root key: %v", err)
	}
	if users, _ := store.ListUsers(context.Background(), ports.UserFilter{IncludeDeleted: true}); len(users) != 3 {
		t.Fatalf("restored users = %d", len(users))
	}

	// 模拟恢复期间 Xray 已重启（动态用户丢失）。
	app.Adapter.Restart()
	app.Adapter.Users["managed"]["bootstrap"] = ports.RemoteUser{StatisticsID: "bootstrap", Present: true, Kind: "bootstrap"}
	node := &sync.Mutex{}
	synchronizer := worker.NewSynchronizer(store, app.Adapter, app.Keyring, app.Clock, nil, node, worker.SynchronizerOptions{RPCTimeout: app.Target.RPCTimeout, Random: func(n int64) int64 { return n - 1 }})
	reconcile := application.NewReconciliationService(store, app.Adapter, app.Clock, app.Target, node, 15*time.Second, synchronizer.Wake, nil, nil)
	summary, err := reconcile.ReconcileOnce(context.Background())
	if err != nil || summary.Drift != 1 {
		t.Fatalf("reconcile after restore = %#v, %v", summary, err)
	}
	if _, err := synchronizer.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	present := func(record ports.UserRecord) bool {
		_, ok := app.Adapter.Users["managed"][record.Identity.StatisticsID]
		return ok
	}
	if !present(active) || present(blocked) || present(deleted) {
		t.Fatalf("after restore active=%v blocked=%v deleted=%v", present(active), present(blocked), present(deleted))
	}
	restoredActive, _ := store.User(context.Background(), active.User.ID)
	if restoredActive.Allocation.ProjectionState != domain.ProjectionPresent || restoredActive.Credential.State != domain.CredentialActive {
		t.Fatalf("restored active allocation = %#v", restoredActive.Allocation)
	}
	restoredBlocked, _ := store.User(context.Background(), blocked.User.ID)
	if restoredBlocked.Cycle.AccountedUplinkBytes != 1<<20 || restoredBlocked.Allocation.QuotaState != domain.QuotaExceeded {
		t.Fatalf("restored quota history lost: %#v", restoredBlocked.Cycle)
	}
}
