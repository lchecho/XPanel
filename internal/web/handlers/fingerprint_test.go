package handlers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"xpanel/internal/testsupport"
)

// secondSession 用独立 cookie jar 以同一管理员再登录一次，模拟另一浏览器会话。
func secondSession(t *testing.T, app *testsupport.App) func(path, tokenPath string, form url.Values) *http.Response {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	csrf := regexp.MustCompile(`name="_csrf" value="([^"]+)"`)
	post := func(path, tokenPath string, form url.Values) *http.Response {
		t.Helper()
		page, err := client.Get(app.Server.URL + tokenPath)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(page.Body)
		page.Body.Close()
		match := csrf.FindStringSubmatch(string(body))
		if len(match) != 2 {
			t.Fatalf("csrf missing on %s", tokenPath)
		}
		form.Set("_csrf", match[1])
		request, _ := http.NewRequest(http.MethodPost, app.Server.URL+path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response
	}
	if response := post("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}}); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("second login status=%d", response.StatusCode)
	}
	return post
}

// T132：请求指纹由 handler 绑定 session+动作+目标+规范化载荷；同会话同载荷重放返回原结果，载荷不同或跨会话返回 409。
func TestCommandFingerprintBindsSessionAndPayload(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	form, _ := profileForm(t, "Fingerprinted", "managed")
	form.Set("_request_id", testsupport.NewID(t).String())
	response, body := app.PostForm("/profiles", "/profiles/new", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d body=%s", response.StatusCode, body)
	}
	location := response.Header.Get("Location")

	// 同会话、同请求、同载荷（CSRF 令牌不同）：幂等重放到原结果。
	replay := url.Values{}
	for key, values := range form {
		replay[key] = values
	}
	replay.Del("_csrf")
	response, body = app.PostForm("/profiles", "/profiles/new", replay)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != location {
		t.Fatalf("replay status=%d location=%q body=%s", response.StatusCode, response.Header.Get("Location"), body)
	}
	profiles, err := app.Profiles.List(context.Background(), false)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles after replay = %d, %v", len(profiles), err)
	}

	// 同请求 ID、不同载荷：拒绝为冲突。
	altered := url.Values{}
	for key, values := range replay {
		altered[key] = values
	}
	altered.Set("name", "Fingerprinted Altered")
	response, _ = app.PostForm("/profiles", "/profiles/new", altered)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("altered payload status=%d", response.StatusCode)
	}

	// 另一会话重放同请求 ID 与同载荷：指纹不匹配，拒绝。
	post := secondSession(t, app)
	other := url.Values{}
	for key, values := range replay {
		other[key] = values
	}
	if response := post("/profiles", "/profiles/new", other); response.StatusCode != http.StatusConflict {
		t.Fatalf("cross-session replay status=%d", response.StatusCode)
	}
	profiles, _ = app.Profiles.List(context.Background(), false)
	if len(profiles) != 1 {
		t.Fatalf("profiles after conflicts = %d", len(profiles))
	}
}

// T132：用户状态变更命令同样携带会话绑定指纹：跨会话重放同一 _request_id 被拒绝。
func TestUserCommandsRejectCrossSessionReplay(t *testing.T) {
	app := testsupport.New(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	user := app.CreateUser("Alice", profileID, nil)
	path := "/users/" + user.User.ID.String()
	form := url.Values{"_request_id": {testsupport.NewID(t).String()}, "_version": {"0"}}
	response, body := app.PostForm(path+"/disable", path, form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable status=%d body=%s", response.StatusCode, body)
	}
	post := secondSession(t, app)
	replay := url.Values{"_request_id": form["_request_id"], "_version": {"0"}}
	if response := post(path+"/disable", path, replay); response.StatusCode != http.StatusConflict {
		t.Fatalf("cross-session replay status=%d", response.StatusCode)
	}
	replay.Del("_csrf")
	response, _ = app.PostForm(path+"/disable", path, replay)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-session replay status=%d", response.StatusCode)
	}
}
