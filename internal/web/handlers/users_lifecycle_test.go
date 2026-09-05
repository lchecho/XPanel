package handlers_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

func TestUserLifecycleActionsIdempotencyAndConflicts(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	record := app.CreateUser("Alice", profileID, nil)
	app.Drain()
	path := "/users/" + record.User.ID.String()
	_, body := app.Get(path)
	if !strings.Contains(body, `action="`+path+`/disable"`) || !strings.Contains(body, "轮换凭证") || !strings.Contains(body, "删除") {
		t.Fatalf("detail actions missing: %s", body)
	}

	requestID := testsupport.NewID(t).String()
	disable := url.Values{"_request_id": {requestID}, "_version": {"0"}}
	response, _ := app.PostForm(path+"/disable", path, disable)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable status=%d", response.StatusCode)
	}
	response, _ = app.PostForm(path+"/disable", path, disable)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("duplicate disable status=%d", response.StatusCode)
	}
	response, body = app.PostForm(path+"/enable", path, url.Values{"_request_id": {requestID}, "_version": {"0"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "该请求已提交过且内容不同") {
		t.Fatalf("mismatched request id status=%d body=%s", response.StatusCode, body)
	}
	response, body = app.PostForm(path+"/enable", path, url.Values{"_version": {"0"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "手动禁用") || !strings.Contains(body, `name="_version" value="1"`) {
		t.Fatalf("stale version status=%d body=%s", response.StatusCode, body)
	}
	app.Drain()
	_, body = app.Get(path)
	if !strings.Contains(body, `action="`+path+`/enable"`) || !strings.Contains(body, "手动禁用") {
		t.Fatalf("detail after disable: %s", body)
	}
	response, _ = app.PostForm(path+"/enable", path, url.Values{"_version": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable status=%d", response.StatusCode)
	}
	app.Drain()

	response, body = app.Get(path + "/rotate")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "旧凭证只有在节点确认移除后才停止接受新连接") {
		t.Fatalf("rotate page status=%d body=%s", response.StatusCode, body)
	}
	response, _ = app.PostForm(path+"/rotate", path+"/rotate", url.Values{"_version": {"2"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("rotate status=%d", response.StatusCode)
	}
	_, body = app.Get(path + "/connection")
	if !strings.Contains(body, "连接信息尚不可用") {
		t.Fatalf("connection during rotation body=%s", body)
	}
	response, body = app.PostForm(path+"/rotate", path+"/rotate", url.Values{"_version": {"3"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "credential rotation already in progress") && !strings.Contains(body, "轮换") {
		t.Fatalf("second rotation status=%d body=%s", response.StatusCode, body)
	}
	app.Drain()
	_, body = app.Get(path + "/connection")
	if !strings.Contains(body, "ss://") {
		t.Fatalf("connection after rotation body=%s", body)
	}

	response, body = app.Get(path + "/delete")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "软删除") {
		t.Fatalf("delete page status=%d body=%s", response.StatusCode, body)
	}
	response, _ = app.PostForm(path+"/delete", path+"/delete", url.Values{"_version": {"3"}})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/users" {
		t.Fatalf("delete status=%d location=%s", response.StatusCode, response.Header.Get("Location"))
	}
	app.Drain()
	_, body = app.Get(path)
	if !strings.Contains(body, "该用户已删除，页面只读") || strings.Contains(body, `action="`+path+`/enable"`) {
		t.Fatalf("deleted detail body=%s", body)
	}
	response, _ = app.PostForm(path+"/enable", path, url.Values{"_version": {"4"}})
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("enable on deleted status=%d", response.StatusCode)
	}
	response, _ = app.Get(path + "/edit")
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("edit deleted status=%d", response.StatusCode)
	}
	if app.User(record.User.ID).User.Lifecycle != domain.LifecycleDeleted {
		t.Fatal("user not deleted")
	}
}
