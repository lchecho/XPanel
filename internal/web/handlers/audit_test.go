package handlers_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

func TestAuditListFiltersPaginatesAndHidesSecrets(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	record := app.CreateUser("Alice", profileID, nil)
	app.Drain()
	action := func(label string) string { return `<td data-label="动作">` + label + `</td>` }
	response, body := app.Get("/audit")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, action("创建用户")) || !strings.Contains(body, action("同步确认")) ||
		!strings.Contains(body, action("登录")) || !strings.Contains(body, "用户 Alice") || strings.Contains(body, app.Password) || strings.Contains(body, "ss://") {
		t.Fatalf("audit page status=%d body=%s", response.StatusCode, body)
	}
	_, body = app.Get("/audit?user=" + record.User.ID.String() + "&action=" + domain.ActionUserCreated)
	if !strings.Contains(body, action("创建用户")) || strings.Contains(body, action("登录")) || strings.Contains(body, action("同步确认")) {
		t.Fatalf("filtered audit body=%s", body)
	}
	if _, body = app.Get("/audit?result=failed"); !strings.Contains(body, "没有符合条件的审计记录") {
		t.Fatalf("empty filtered result body=%s", body)
	}
	for i := 0; i < 60; i++ {
		id := testsupport.NewID(t)
		if err := app.Store.AppendAudit(context.Background(), domain.AuditEvent{ID: id, OccurredAt: app.Clock.Now(), ActorType: domain.ActorSystem,
			TargetType: "user", TargetID: record.User.ID, Action: domain.ActionSyncSucceeded, Result: domain.AuditSucceeded, SafeSummary: "bulk"}); err != nil {
			t.Fatal(err)
		}
	}
	_, body = app.Get("/audit?user=" + record.User.ID.String())
	next := regexp.MustCompile(`href="(/audit\?[^"]*cursor=[^"]+)"`).FindStringSubmatch(body)
	if len(next) != 2 || strings.Count(body, "<tr><td data-label=\"时间\">") != 50 {
		t.Fatalf("first page rows=%d next=%v", strings.Count(body, "<tr><td data-label=\"时间\">"), next)
	}
	_, body = app.Get(strings.ReplaceAll(next[1], "&amp;", "&"))
	if rows := strings.Count(body, "<tr><td data-label=\"时间\">"); rows == 0 || rows > 50 || strings.Contains(body, "cursor=") && strings.Count(body, "下一页") > 1 {
		t.Fatalf("second page rows=%d body=%s", rows, body)
	}
	anonymous := testsupport.New(t)
	if response, _ = anonymous.Get("/audit"); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous audit status=%d", response.StatusCode)
	}
}
