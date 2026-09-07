package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// 端口分配是「一用户一端口」的基础：分配 MUST 唯一、确定，失败 MUST 不留部分状态（FR-006/FR-007/FR-008）。
func TestPortsAreAssignedUniquelyAndDeterministically(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Primary", 31000, 31004)

	// 自动分配按升序取池内最小空闲端口，结果可预期。
	var assigned []int
	for i := 0; i < 3; i++ {
		record := app.CreateUser("Auto "+string(rune('A'+i)), templateID, nil)
		assigned = append(assigned, record.Inbound.Inbound.Port)
	}
	for i, port := range assigned {
		if port != 31000+i {
			t.Fatalf("auto-assigned ports = %v, want ascending from 31000", assigned)
		}
	}

	// 删除中间的用户后端口回到池中，下一次分配填补该空洞而不是继续向后取。
	middle := app.User(app.ListUsers()[1].User.ID)
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: middle.User.ID,
		ExpectedRevision: middle.User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	refilled := app.CreateUser("Refill", templateID, nil)
	if refilled.Inbound.Inbound.Port != middle.Inbound.Inbound.Port {
		t.Fatalf("released port %d was not reused: got %d", middle.Inbound.Inbound.Port, refilled.Inbound.Inbound.Port)
	}
}

// 指定端口：池内空闲端口被接受，已占用端口冲突，池外端口是字段级校验错误；两种失败都不得留下部分状态。
func TestRequestedPortsAreValidatedWithoutPartialState(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Primary", 32000, 32003)
	chosen := 32002
	explicit, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Explicit",
		TemplateID: templateID, Port: &chosen, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	if err != nil {
		t.Fatal(err)
	}
	if port := app.User(explicit).Inbound.Inbound.Port; port != chosen {
		t.Fatalf("requested port = %d want %d", port, chosen)
	}
	app.Drain()

	before := len(app.ListUsers())
	taken := chosen
	_, _, err = app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Conflict",
		TemplateID: templateID, Port: &taken, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("occupied port error = %T %v", err, err)
	}
	outside := 40000
	_, _, err = app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Outside",
		TemplateID: templateID, Port: &outside, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) || invalid.Field != "port" {
		t.Fatalf("out-of-pool port error = %T %v", err, err)
	}
	if after := len(app.ListUsers()); after != before {
		t.Fatalf("rejected creations left %d extra users", after-before)
	}
	if app.Drain() != 0 {
		t.Fatal("rejected creations enqueued synchronization work")
	}
}

// 端口池耗尽是冲突而不是内部错误，并且不得留下任何部分状态。
func TestPortPoolExhaustionIsAConflictWithoutPartialState(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Tiny", 33000, 33001)
	app.CreateUser("One", templateID, nil)
	app.CreateUser("Two", templateID, nil)
	before := len(app.ListUsers())
	_, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: "Third",
		TemplateID: templateID, ResetDay: 1, RequestID: testsupport.NewID(t), ActorID: app.AdminID})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Message != domain.ErrPortPoolExhausted.Message {
		t.Fatalf("exhausted pool error = %T %v", err, err)
	}
	if after := len(app.ListUsers()); after != before {
		t.Fatalf("exhausted creation left %d extra users", after-before)
	}
	app.Drain()
	if listening := app.Listening(33000) && app.Listening(33001); !listening {
		t.Fatal("the two accepted users are not both listening")
	}
}

// 并发创建：数据库唯一约束是端口唯一性的最终保证，任何并发组合都不得让两个用户拿到同一端口。
func TestConcurrentCreationNeverAssignsTheSamePortTwice(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterTemplateWithPool("Concurrent", 34000, 34015)
	const attempts = 16
	var wait sync.WaitGroup
	results := make([]domain.ID, attempts)
	failures := make([]error, attempts)
	wait.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(index int) {
			defer wait.Done()
			id, _, err := app.Users.CreateUser(context.Background(), application.CreateUserInput{
				DisplayName: "Racer " + string(rune('A'+index)), TemplateID: templateID, ResetDay: 1,
				RequestID: testsupport.NewID(t), ActorID: app.AdminID})
			results[index], failures[index] = id, err
		}(i)
	}
	wait.Wait()

	seen := map[int]string{}
	created := 0
	for i, id := range results {
		if failures[i] != nil {
			// 允许因端口竞争而失败，但失败必须是可理解的冲突而不是内部错误。
			var conflict *domain.ConflictError
			if !errors.As(failures[i], &conflict) {
				t.Fatalf("concurrent creation #%d failed with %T %v", i, failures[i], failures[i])
			}
			continue
		}
		created++
		record := app.User(id)
		port := record.Inbound.Inbound.Port
		if owner, clash := seen[port]; clash {
			t.Fatalf("port %d assigned to both %s and %s", port, owner, record.User.DisplayName)
		}
		seen[port] = record.User.DisplayName
	}
	if created == 0 {
		t.Fatal("no concurrent creation succeeded")
	}
	app.Drain()
	for port := range seen {
		if !app.Listening(port) {
			t.Fatalf("port %d did not converge to listening", port)
		}
	}
}
