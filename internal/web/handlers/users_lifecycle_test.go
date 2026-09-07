package handlers_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

func TestUserLifecycleActionsIdempotencyAndConflicts(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	path := "/users/" + record.User.ID.String()
	port := strconv.Itoa(record.Inbound.Inbound.Port)
	_, body := app.Get(path)
	if !strings.Contains(body, `action="`+path+`/disable"`) || !strings.Contains(body, "轮换凭证") || !strings.Contains(body, "删除") {
		t.Fatalf("detail actions missing: %s", body)
	}
	// 四条生命周期路径都必须在详情页反映端口与监听状态。
	assertListening := func(step, state string) {
		t.Helper()
		_, detail := app.Get(path)
		if !strings.Contains(detail, "<dt>专属端口</dt><dd>"+port+"</dd>") {
			t.Fatalf("%s: detail lost the port %s: %s", step, port, detail)
		}
		if !strings.Contains(detail, "<dt>监听状态</dt><dd>"+state+"</dd>") {
			t.Fatalf("%s: listening state is not %q: %s", step, state, detail)
		}
	}
	assertListening("created", "监听中")

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
	assertListening("disabled", "未监听")
	response, _ = app.PostForm(path+"/enable", path, url.Values{"_version": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable status=%d", response.StatusCode)
	}
	app.Drain()
	assertListening("re-enabled", "监听中")

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
	if !strings.Contains(body, ":"+port) {
		t.Fatalf("connection information does not carry the dedicated port %s: %s", port, body)
	}
	assertListening("rotated", "监听中")

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
	assertListening("deleted", "未监听")
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

// FR-010：端口被面板外进程占用时，管理员可以在界面上直接换端口。
func TestChangePortFormAndConflicts(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterTemplateWithPool("Primary", 39000, 39003)
	record := app.CreateUser("Alice", templateID, nil)
	other := app.CreateUser("Bob", templateID, nil)
	app.Drain()
	path := "/users/" + record.User.ID.String()

	_, body := app.Get(path)
	if !strings.Contains(body, `href="`+path+`/port"`) {
		t.Fatalf("detail page has no change-port entry: %s", body)
	}
	response, body := app.Get(path + "/port")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "端口池范围 39000–39003") ||
		!strings.Contains(body, "端口变更必然中断监听") {
		t.Fatalf("change-port form status=%d body=%s", response.StatusCode, body)
	}

	// 池外端口是 422，页面保留输入并给出允许范围。
	response, body = app.PostForm(path+"/port", path+"/port", url.Values{"port": {"40000"}, "_version": {"0"}})
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "端口必须位于端口池内") ||
		!strings.Contains(body, `value="40000"`) {
		t.Fatalf("out-of-pool port status=%d body=%s", response.StatusCode, body)
	}
	// 非数字同样是字段级错误。
	if response, _ = app.PostForm(path+"/port", path+"/port", url.Values{"port": {"abc"}, "_version": {"0"}}); response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("non-numeric port status=%d", response.StatusCode)
	}
	// 已被其他用户占用的端口是 409。
	taken := strconv.Itoa(app.User(other.User.ID).Inbound.Inbound.Port)
	response, body = app.PostForm(path+"/port", path+"/port", url.Values{"port": {taken}, "_version": {"0"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "该端口已分配给其他用户") {
		t.Fatalf("occupied port status=%d body=%s", response.StatusCode, body)
	}

	// 成功：详情页展示新端口，且状态回到监听中。
	response, _ = app.PostForm(path+"/port", path+"/port", url.Values{"port": {"39002"}, "_version": {"0"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("change status=%d", response.StatusCode)
	}
	app.Drain()
	_, body = app.Get(path)
	if !strings.Contains(body, "<dt>专属端口</dt><dd>39002</dd>") || !strings.Contains(body, "<dt>监听状态</dt><dd>监听中</dd>") {
		t.Fatalf("detail after the port change: %s", body)
	}
	// 陈旧版本再次提交是冲突。
	response, _ = app.PostForm(path+"/port", path+"/port", url.Values{"port": {"39003"}, "_version": {"0"}})
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale version status=%d", response.StatusCode)
	}
}
