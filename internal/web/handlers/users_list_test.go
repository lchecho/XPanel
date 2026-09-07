package handlers_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/testsupport"
)

func TestUserListSearchAndStatusFilters(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	alice := app.CreateUser("Alice", templateID, nil)
	bob := app.CreateUser("Bob", templateID, nil)
	carol := app.CreateUser("Carol", templateID, nil)
	app.Drain()
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: bob.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Users.DeleteUser(context.Background(), application.LifecycleInput{ID: carol.User.ID, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	cases := []struct {
		query   string
		include []string
		exclude []string
	}{
		{"", []string{"Alice", "Bob", "受管用户（2）"}, []string{"Carol"}},
		{"?status=disabled", []string{"Bob", "手动禁用"}, []string{"Alice", "Carol"}},
		{"?status=active", []string{"Alice"}, []string{"Bob", "Carol"}},
		{"?q=ali", []string{"Alice"}, []string{"Bob"}},
		{"?deleted=1", []string{"Alice", "Bob", "Carol", "受管用户（3）"}, nil},
		{"?status=deleted", []string{"Carol", "已删除"}, []string{"Alice", "Bob"}},
		{"?q=nobody", []string{"没有符合条件的用户"}, []string{"Alice"}},
	}
	for _, c := range cases {
		response, body := app.Get("/users" + c.query)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", c.query, response.StatusCode)
		}
		for _, want := range c.include {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q: %s", c.query, want, body)
			}
		}
		for _, unwanted := range c.exclude {
			if strings.Contains(body, ">"+unwanted+"<") {
				t.Fatalf("%s unexpectedly contains %q", c.query, unwanted)
			}
		}
	}
	_, body := app.Get("/users")
	if !strings.Contains(body, `<caption>`) || !strings.Contains(body, `scope="col"`) || !strings.Contains(body, alice.User.ID.String()) {
		t.Fatalf("table structure missing: %s", body)
	}
}

// FR-014：端口是用户在节点上的唯一标识，列表必须能按端口反查并展示端口与监听状态。
func TestUserListSearchesByPortAndShowsListeningState(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	alice := app.CreateUser("Alice", templateID, nil)
	bob := app.CreateUser("Bob", templateID, nil)
	app.Drain()
	alicePort := strconv.Itoa(app.User(alice.User.ID).Inbound.Inbound.Port)
	bobPort := strconv.Itoa(app.User(bob.User.ID).Inbound.Inbound.Port)

	_, body := app.Get("/users")
	if !strings.Contains(body, `<th scope="col">端口</th>`) || !strings.Contains(body, `<td data-label="端口">`+alicePort+`</td>`) ||
		!strings.Contains(body, `<td data-label="监听">监听中</td>`) {
		t.Fatalf("list does not show ports and listening state: %s", body)
	}
	_, body = app.Get("/users?q=" + alicePort)
	if !strings.Contains(body, "Alice") || strings.Contains(body, ">Bob<") {
		t.Fatalf("search by port %s returned the wrong rows: %s", alicePort, body)
	}
	_, body = app.Get("/users?q=" + bobPort)
	if !strings.Contains(body, "Bob") || strings.Contains(body, ">Alice<") {
		t.Fatalf("search by port %s returned the wrong rows: %s", bobPort, body)
	}
	// 名称搜索不受端口搜索影响。
	_, body = app.Get("/users?q=Ali")
	if !strings.Contains(body, "Alice") || strings.Contains(body, ">Bob<") {
		t.Fatalf("search by name broke: %s", body)
	}
	// 禁用后列表显示未监听。
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: alice.User.ID, Enabled: false,
		ExpectedRevision: app.User(alice.User.ID).User.Revision, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	_, body = app.Get("/users?q=" + alicePort)
	if !strings.Contains(body, `<td data-label="监听">未监听</td>`) {
		t.Fatalf("disabled user is not reported as not listening: %s", body)
	}
}
