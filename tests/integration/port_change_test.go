package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// FR-010：端口被面板外进程占用时，管理员可以直接换端口，而不必等占用解除。
func TestChangingThePortMovesTheInboundAndReleasesTheOldPort(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	oldPort := record.Inbound.Inbound.Port
	tag := record.Inbound.Inbound.InboundTag
	before, err := app.Connections.BuildConnectionInfo(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 外部进程占用该端口：入站变得不可重建，用户保持待同步。
	app.Adapter.ExternalPorts[oldPort] = true
	if _, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: record.User.ID,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()

	newPort := oldPort + 7
	if _, err := app.Users.ChangePort(context.Background(), application.ChangePortInput{ID: record.User.ID, Port: newPort,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()

	after := app.User(record.User.ID)
	if after.Inbound.Inbound.Port != newPort || !app.Listening(newPort) {
		t.Fatalf("port after change = %d want %d listening", after.Inbound.Inbound.Port, newPort)
	}
	if after.Allocation.PendingSync() {
		t.Fatalf("allocation still pending after the port change: %#v", after.Allocation)
	}
	// 入站标签、统计身份与流量历史都不变；旧端口不再由面板占用。
	if after.Inbound.Inbound.InboundTag != tag || after.Identity.StatisticsID != record.Identity.StatisticsID {
		t.Fatalf("port change moved the identity: %#v", after.Inbound.Inbound)
	}
	for _, inbound := range app.Adapter.Inbounds {
		if inbound.Port == oldPort && domain.IsPanelNamespace(inbound.Tag) {
			t.Fatalf("a panel inbound still holds the old port %d", oldPort)
		}
	}
	// 连接信息换了端口，必须重新交付。
	updated, err := app.Connections.BuildConnectionInfo(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Port != newPort || updated.URI.Reveal() == before.URI.Reveal() {
		t.Fatalf("connection information did not follow the port: %d", updated.Port)
	}
	// 审计记录了旧端口与新端口，且不含密钥。
	var summary string
	if err := app.Store.DB().Read.QueryRow(`SELECT safe_summary FROM audit_events WHERE action=? AND target_id=?`,
		domain.ActionPortChanged, record.User.ID.String()).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "30000") || !strings.Contains(summary, "30007") || strings.Contains(summary, "ss://") {
		t.Fatalf("port change audit summary = %q", summary)
	}
}

// 校验与冲突：池外端口是字段级错误，已占用端口是冲突，两者都不得留下部分状态。
func TestChangingThePortValidatesWithoutPartialState(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Primary", 37000, 37003)
	first := app.CreateUser("First", templateID, nil)
	second := app.CreateUser("Second", templateID, nil)
	app.Drain()
	firstPort := first.Inbound.Inbound.Port

	outside := 40000
	_, err := app.Users.ChangePort(context.Background(), application.ChangePortInput{ID: first.User.ID, Port: outside,
		ExpectedRevision: app.User(first.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) || invalid.Field != "port" {
		t.Fatalf("out-of-pool change = %T %v", err, err)
	}
	taken := second.Inbound.Inbound.Port
	_, err = app.Users.ChangePort(context.Background(), application.ChangePortInput{ID: first.User.ID, Port: taken,
		ExpectedRevision: app.User(first.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("occupied change = %T %v", err, err)
	}
	// 陈旧 revision 也是冲突。
	_, err = app.Users.ChangePort(context.Background(), application.ChangePortInput{ID: first.User.ID, Port: 37002,
		ExpectedRevision: 99, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if !errors.As(err, &conflict) {
		t.Fatalf("stale revision change = %T %v", err, err)
	}
	// 三次拒绝都没有改变任何状态。
	if current := app.User(first.User.ID); current.Inbound.Inbound.Port != firstPort || current.Allocation.PendingSync() {
		t.Fatalf("rejected changes left partial state: %#v", current.Inbound.Inbound)
	}
	if app.Drain() != 0 {
		t.Fatal("rejected changes enqueued synchronization work")
	}
	if !app.Listening(firstPort) || !app.Listening(taken) {
		t.Fatal("rejected changes disturbed the listening ports")
	}
}

// 重复提交同一请求只产生一条意图；并发换到同一端口只有一个能成功。
func TestChangingThePortIsIdempotentAndRaceFree(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Primary", 38000, 38009)
	first := app.CreateUser("First", templateID, nil)
	second := app.CreateUser("Second", templateID, nil)
	app.Drain()

	requestID := testsupport.NewID(t)
	revision := app.User(first.User.ID).User.Revision
	for i := 0; i < 2; i++ {
		if _, err := app.Users.ChangePort(context.Background(), application.ChangePortInput{ID: first.User.ID, Port: 38005,
			ExpectedRevision: revision, RequestID: requestID, ActorID: app.AdminID}); err != nil {
			t.Fatalf("duplicate submission #%d: %v", i, err)
		}
	}
	var operations int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason='port_change'`,
		first.Allocation.ID.String()).Scan(&operations)
	if operations != 1 {
		t.Fatalf("duplicate request produced %d operations", operations)
	}
	app.Drain()

	// 两个用户同时抢同一个空闲端口：数据库唯一索引保证只有一个成功。
	var wait sync.WaitGroup
	results := make([]error, 2)
	targets := []domain.ID{first.User.ID, second.User.ID}
	revisions := []domain.Revision{app.User(first.User.ID).User.Revision, app.User(second.User.ID).User.Revision}
	wait.Add(2)
	for i := 0; i < 2; i++ {
		go func(index int) {
			defer wait.Done()
			_, results[index] = app.Users.ChangePort(context.Background(), application.ChangePortInput{ID: targets[index],
				Port: 38008, ExpectedRevision: revisions[index], RequestID: testsupport.NewID(t), ActorID: app.AdminID})
		}(i)
	}
	wait.Wait()
	succeeded := 0
	for i, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		var conflict *domain.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("racing change #%d failed with %T %v", i, err, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("racing changes succeeded %d times, want exactly 1", succeeded)
	}
	app.Drain()
	assigned := map[int]bool{}
	for _, id := range targets {
		port := app.User(id).Inbound.Inbound.Port
		if assigned[port] {
			t.Fatalf("both users ended on port %d", port)
		}
		assigned[port] = true
		if !app.Listening(port) {
			t.Fatalf("port %d is not listening after the race", port)
		}
	}
}
