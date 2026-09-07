package integration

import (
	"context"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

type cycleFacts struct {
	open       int
	closedUp   int64
	restoreOps int
}

func inspectCycles(t *testing.T, app *testsupport.App, allocationID domain.ID) cycleFacts {
	t.Helper()
	var facts cycleFacts
	if err := app.Store.DB().Read.QueryRow(`SELECT count(*) FROM quota_cycles WHERE allocation_id=? AND status='open'`, allocationID.String()).Scan(&facts.open); err != nil {
		t.Fatal(err)
	}
	_ = app.Store.DB().Read.QueryRow(`SELECT COALESCE(SUM(accounted_uplink_bytes),0) FROM quota_cycles WHERE allocation_id=? AND status='closed'`, allocationID.String()).Scan(&facts.closedUp)
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM synchronization_operations WHERE allocation_id=? AND reason='quota_restore'`, allocationID.String()).Scan(&facts.restoreOps)
	return facts
}

func blockedUser(t *testing.T, app *testsupport.App, templateID domain.ID, name string, limit int64) ports.UserRecord {
	t.Helper()
	user := app.CreateUser(name, templateID, &limit)
	app.Drain()
	app.SetTraffic(user, uint64(limit), 0)
	if summary := app.Collect(); summary.Blocked != 1 {
		t.Fatalf("block summary = %#v", summary)
	}
	app.Drain()
	record := app.User(user.User.ID)
	if record.Allocation.QuotaState != domain.QuotaExceeded || record.Allocation.ProjectionState != domain.ProjectionAbsent {
		t.Fatalf("user not blocked: %#v", record.Allocation)
	}
	return record
}

// T145 顺序 A：先由 scheduler 切换周期，再采集跨边界后的增量；恢复操作恰好一个，增量只进入新周期。
func TestCycleBoundaryRolloverThenCollect(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	record := blockedUser(t, app, templateID, "Blocked", limit)
	boundary := record.Cycle.EndsAt
	app.Clock.Set(boundary.Add(time.Second))
	if app.Rollover() != 1 {
		t.Fatal("expected exactly one boundary")
	}
	app.Drain()
	app.SetTraffic(record, uint64(limit)+5000, 0)
	if summary := app.Collect(); summary.Applied != 1 || summary.Rolled != 0 || summary.Blocked != 0 {
		t.Fatalf("collect summary = %#v", summary)
	}
	after := app.User(record.User.ID)
	facts := inspectCycles(t, app, record.Allocation.ID)
	if facts.open != 1 || !after.Cycle.StartsAt.Equal(boundary) || after.Cycle.AccountedUplinkBytes != 5000 || facts.closedUp != limit || facts.restoreOps != 1 {
		t.Fatalf("after rollover-then-collect: cycle=%#v facts=%#v", after.Cycle, facts)
	}
	if after.Allocation.ProjectionState != domain.ProjectionPresent || after.Allocation.QuotaState != domain.QuotaWithinLimit {
		t.Fatalf("allocation = %#v", after.Allocation)
	}
	if app.Rollover() != 0 {
		t.Fatal("second rollover crossed a boundary again")
	}
}

// T145 顺序 B：样本完成时间已跨边界但 scheduler 尚未运行：采集提交在同一事务内结算周期，增量进入新周期，之后的切换无事可做。
func TestCycleBoundaryCollectThenRollover(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	active := app.CreateUser("Active", templateID, &limit)
	app.Drain()
	app.SetTraffic(active, 1000, 0)
	app.Collect()
	blocked := blockedUser(t, app, templateID, "Blocked", limit)
	boundary := app.User(active.User.ID).Cycle.EndsAt
	app.Clock.Set(boundary.Add(time.Second))
	app.SetTraffic(active, 6000, 0) // 边界后增长 5000
	summary := app.Collect()
	if summary.Applied != 1 || summary.Rolled != 1 || summary.Restored != 0 || summary.Blocked != 0 {
		t.Fatalf("collect summary = %#v", summary)
	}
	after := app.User(active.User.ID)
	facts := inspectCycles(t, app, active.Allocation.ID)
	if facts.open != 1 || !after.Cycle.StartsAt.Equal(boundary) || after.Cycle.AccountedUplinkBytes != 5000 || facts.closedUp != 1000 {
		t.Fatalf("collect-then-rollover: cycle=%#v facts=%#v", after.Cycle, facts)
	}
	var total int64
	_ = app.Store.DB().Read.QueryRow(`SELECT uplink_bytes FROM allocation_traffic_totals WHERE allocation_id=?`, active.Allocation.ID.String()).Scan(&total)
	if total != 6000 {
		t.Fatalf("lifetime total = %d, want 6000 (counted once)", total)
	}
	// 被封禁的用户不在采集目标内，由 scheduler 结算并恢复；活跃用户的周期已结算，不再重复切换。
	if app.Rollover() != 1 {
		t.Fatal("scheduler should roll exactly the blocked user's cycle")
	}
	app.Drain()
	blockedAfter := app.User(blocked.User.ID)
	blockedFacts := inspectCycles(t, app, blocked.Allocation.ID)
	if blockedFacts.open != 1 || blockedFacts.restoreOps != 1 || blockedAfter.Allocation.ProjectionState != domain.ProjectionPresent {
		t.Fatalf("blocked user after scheduler: %#v facts=%#v", blockedAfter.Allocation, blockedFacts)
	}
	if facts = inspectCycles(t, app, active.Allocation.ID); facts.open != 1 || facts.restoreOps != 0 {
		t.Fatalf("active user cycles after scheduler: %#v", facts)
	}
}

// T145：边界前修改 reset_day 与面板时区，新周期按事务内读到的最新策略与时区计算。
func TestCycleBoundaryUsesLatestPolicyAndTimezone(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	user := app.CreateUser("Policy", templateID, nil)
	app.Drain()
	if _, err := app.Users.UpdateUser(context.Background(), application.UpdateUserInput{ID: user.User.ID, DisplayName: "Policy", LimitBytes: nil, ResetDay: 15,
		AdminEnabled: true, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	settings, _ := app.Store.Settings(context.Background())
	if _, err := app.Settings.Update(context.Background(), application.UpdateSettingsInput{QuotaTimezone: "Asia/Shanghai", ExpectedRevision: domain.Revision(settings.Revision),
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	boundary := app.User(user.User.ID).Cycle.EndsAt
	app.Clock.Set(boundary.Add(time.Minute))
	if app.Rollover() != 1 {
		t.Fatal("expected one boundary")
	}
	after := app.User(user.User.ID)
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	wantEnd := time.Date(boundary.In(shanghai).Year(), boundary.In(shanghai).Month(), 15, 0, 0, 0, 0, shanghai)
	if !wantEnd.After(boundary) {
		wantEnd = wantEnd.AddDate(0, 1, 0)
	}
	if !after.Cycle.StartsAt.Equal(boundary) || !after.Cycle.EndsAt.Equal(wantEnd) || after.Cycle.ResetDay != 15 || after.Cycle.Timezone != "Asia/Shanghai" {
		t.Fatalf("new cycle = %#v want end %s", after.Cycle, wantEnd)
	}
}

// T145：禁用用户跨边界不得恢复；边界后对已关闭周期的手动重置返回冲突，对新周期的重置正常。
func TestCycleBoundaryDisabledUserAndStaleReset(t *testing.T) {
	app := testsupport.New(t)
	templateID := app.RegisterCompatibleTemplate("Primary")
	limit := int64(1 << 20)
	record := blockedUser(t, app, templateID, "Disabled", limit)
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: false,
		ExpectedRevision: app.User(record.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	oldCycle := app.User(record.User.ID).Cycle
	app.Clock.Set(oldCycle.EndsAt.Add(time.Second))
	if app.Rollover() != 1 {
		t.Fatal("expected one boundary")
	}
	app.Drain()
	after := app.User(record.User.ID)
	facts := inspectCycles(t, app, record.Allocation.ID)
	if facts.restoreOps != 0 || after.Allocation.ProjectionState != domain.ProjectionAbsent || after.Allocation.QuotaState != domain.QuotaWithinLimit || facts.open != 1 {
		t.Fatalf("disabled user restored across boundary: %#v facts=%#v", after.Allocation, facts)
	}
	// 边界后的手动重置作用于新的 open 周期；已关闭周期的 accounted 值保持不变（重置事件只记录新周期）。
	if _, err := app.Users.ResetTraffic(context.Background(), application.ResetTrafficInput{ID: record.User.ID, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatalf("reset of the open cycle: %v", err)
	}
	facts = inspectCycles(t, app, record.Allocation.ID)
	if facts.closedUp != limit || facts.open != 1 || app.User(record.User.ID).Cycle.ManualResetCount != 1 {
		t.Fatalf("reset touched the closed cycle: facts=%#v cycle=%#v", facts, app.User(record.User.ID).Cycle)
	}
}
