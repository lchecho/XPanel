package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/testsupport"
)

// faultAuthStore 让下一次认证审计写事务失败。
type faultAuthStore struct {
	ports.AuthStore
	mu       sync.Mutex
	failNext bool
}

func (f *faultAuthStore) FailNextWrite() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = true
}

func (f *faultAuthStore) WithWriteTx(ctx context.Context, fn func(ports.WriteTx) error) error {
	f.mu.Lock()
	fail := f.failNext
	f.failNext = false
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	return f.AuthStore.WithWriteTx(ctx, fn)
}

// faultSessionStore 让下一次会话提交或撤销失败。
type faultSessionStore struct {
	scs.Store
	mu         sync.Mutex
	failCommit bool
	failDelete bool
}

func (f *faultSessionStore) FailNextCommit() { f.mu.Lock(); f.failCommit = true; f.mu.Unlock() }
func (f *faultSessionStore) FailNextDelete() { f.mu.Lock(); f.failDelete = true; f.mu.Unlock() }

func (f *faultSessionStore) Commit(token string, data []byte, expiry time.Time) error {
	f.mu.Lock()
	fail := f.failCommit
	f.failCommit = false
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	return f.Store.Commit(token, data, expiry)
}

func (f *faultSessionStore) Delete(token string) error {
	f.mu.Lock()
	fail := f.failDelete
	f.failDelete = false
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	return f.Store.Delete(token)
}

func newFaultyAuthApp(t *testing.T) (*testsupport.App, *faultAuthStore, *faultSessionStore) {
	t.Helper()
	auth := &faultAuthStore{}
	sessions := &faultSessionStore{}
	app := testsupport.NewWith(t, testsupport.Options{
		WrapAuthStore:    func(inner ports.AuthStore) ports.AuthStore { auth.AuthStore = inner; return auth },
		WrapSessionStore: func(inner scs.Store) scs.Store { sessions.Store = inner; return sessions },
	})
	return app, auth, sessions
}

func auditCount(t *testing.T, app *testsupport.App, action string, result domain.AuditResult) int {
	t.Helper()
	var n int
	if err := app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result=?`, action, result).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func loggedIn(app *testsupport.App) bool {
	response, _ := app.Get("/users")
	return response.StatusCode == http.StatusOK
}

func assertNoCredentialLeak(t *testing.T, app *testsupport.App) {
	t.Helper()
	// 用户名 "admin" 是 "administrator" 的子串，改为检查密码、错误尝试的用户名与密码字面量。
	var leaked int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE safe_summary LIKE ? OR safe_summary LIKE '%nobody%' OR safe_summary LIKE '%wrong-password%'`,
		"%"+app.Password+"%").Scan(&leaked)
	if leaked != 0 {
		t.Fatalf("audit leaked username or password: %d rows", leaked)
	}
}

// T147：失败/限流登录的审计写入失败不得被吞掉——请求以 500 拒绝，不建立会话。
func TestFailedLoginAuditWriteFailureIsNotSwallowed(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	auth.FailNextWrite()
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {"nobody"}, "password": {"wrong-password-attempt"}})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed login with audit failure status=%d, want 500", response.StatusCode)
	}
	if auditCount(t, app, domain.ActionLogin, domain.AuditFailed) != 0 || loggedIn(app) {
		t.Fatal("audit or session state inconsistent after audit write failure")
	}
	for i := 0; i < 5; i++ {
		app.PostForm("/login", "/login", url.Values{"username": {"nobody"}, "password": {"wrong-password-attempt"}})
	}
	auth.FailNextWrite()
	response, _ = app.PostForm("/login", "/login", url.Values{"username": {"nobody"}, "password": {"wrong-password-attempt"}})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("throttled login with audit failure status=%d, want 500", response.StatusCode)
	}
	assertNoCredentialLeak(t, app)
}

// T147：会话提交失败时登录不得返回成功，不留下已登录会话，审计为 failed 而非 succeeded。
func TestLoginSessionCommitFailureRecordsFailedLogin(t *testing.T) {
	app, _, sessions := newFaultyAuthApp(t)
	sessions.FailNextCommit()
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("login with session commit failure status=%d, want 500", response.StatusCode)
	}
	if loggedIn(app) {
		t.Fatal("session established although commit failed")
	}
	if auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 0 {
		t.Fatal("succeeded login audited without a persisted session")
	}
	var sessionFailures int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result='failed' AND safe_summary LIKE '%session%'`, domain.ActionLogin).Scan(&sessionFailures)
	if sessionFailures != 1 {
		t.Fatalf("session failure audits = %d, want 1", sessionFailures)
	}
	app.Login()
	if !loggedIn(app) || auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 1 {
		t.Fatal("subsequent login did not establish a session with exactly one succeeded audit")
	}
	assertNoCredentialLeak(t, app)
}

// T147：成功审计写入失败时登录同样不得返回成功，也不得留下已登录会话。
func TestLoginSuccessAuditFailureLeavesNoSession(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	auth.FailNextWrite()
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("login with success-audit failure status=%d, want 500", response.StatusCode)
	}
	if loggedIn(app) || auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 0 {
		t.Fatal("session or succeeded audit left behind after audit failure")
	}
	var live int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM admin_sessions WHERE revoked_at IS NULL AND data LIKE '%administrator_id%'`).Scan(&live)
	if live != 0 {
		t.Fatalf("live authenticated sessions = %d", live)
	}
}

// T147：登出时会话撤销失败——HTTP 500、会话仍有效、审计为 failed，没有 succeeded。
func TestLogoutSessionDeleteFailureKeepsSessionAndAuditsFailure(t *testing.T) {
	app, _, sessions := newFaultyAuthApp(t)
	app.Login()
	sessions.FailNextDelete()
	response, _ := app.PostForm("/logout", "/", nil)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("logout with delete failure status=%d, want 500", response.StatusCode)
	}
	if !loggedIn(app) {
		t.Fatal("session vanished although revocation failed")
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditFailed) != 1 || auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 0 {
		t.Fatalf("logout audits failed=%d succeeded=%d", auditCount(t, app, domain.ActionLogout, domain.AuditFailed), auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded))
	}
	if response, _ = app.PostForm("/logout", "/", nil); response.StatusCode != http.StatusSeeOther || loggedIn(app) {
		t.Fatalf("retry logout status=%d loggedIn=%v", response.StatusCode, loggedIn(app))
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 1 {
		t.Fatal("successful retry not audited exactly once")
	}
}

// T147：登出审计写入失败——会话已撤销，但 HTTP 不返回成功，也没有 succeeded 审计。
func TestLogoutAuditFailureDoesNotReportSuccess(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	app.Login()
	auth.FailNextWrite()
	response, _ := app.PostForm("/logout", "/", nil)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("logout with audit failure status=%d, want 500", response.StatusCode)
	}
	if loggedIn(app) {
		t.Fatal("session still valid after confirmed revocation")
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 0 {
		t.Fatal("succeeded logout audited although the write failed")
	}
	assertNoCredentialLeak(t, app)
}

func hasSessionCookie(app *testsupport.App, response *http.Response) bool {
	for _, value := range response.Header.Values("Set-Cookie") {
		if strings.HasPrefix(value, app.Sessions.Cookie.Name+"=") && !strings.Contains(value, "Max-Age=-1") && !strings.Contains(value, "Max-Age=0") {
			return true
		}
	}
	return false
}

// T148：登录后的普通请求由中间件提交会话（写 flash）；提交失败返回 500、不写会话 cookie，此前已建立的会话保持可用。
func TestMiddlewareSessionCommitFailureLeavesNoPartialState(t *testing.T) {
	app, _, sessions := newFaultyAuthApp(t)
	app.Login()
	settings, _ := app.Store.Settings(context.Background())
	// scs 在配置了 idle timeout 时每次加载都会重新提交会话（滑动过期），因此先取 CSRF 令牌再注入提交故障。
	csrf := app.CSRF("/settings")
	sessions.FailNextCommit()
	response, _ := app.PostForm("/settings", "/settings", url.Values{"quota_timezone": {"Asia/Tokyo"}, "_version": {fmt.Sprint(settings.Revision)}, "_csrf": {csrf}})
	if response.StatusCode != http.StatusInternalServerError || hasSessionCookie(app, response) {
		t.Fatalf("middleware commit failure status=%d cookie=%v", response.StatusCode, hasSessionCookie(app, response))
	}
	if !loggedIn(app) {
		t.Fatal("previously established session lost after a failed middleware commit")
	}
	settings, _ = app.Store.Settings(context.Background())
	response, _ = app.PostForm("/settings", "/settings", url.Values{"quota_timezone": {"Asia/Seoul"}, "_version": {fmt.Sprint(settings.Revision)}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("retry status=%d", response.StatusCode)
	}
}

// T148：成功登录的审计写入失败且当前会话撤销也失败：cookie 被撤回、管理员全部会话被撤销、响应 500、无 succeeded 审计。
func TestLoginAuditFailureWithSessionDeleteFailureRevokesEverything(t *testing.T) {
	app, auth, sessions := newFaultyAuthApp(t)
	auth.FailNextWrite()
	sessions.FailNextDelete()
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}})
	if response.StatusCode != http.StatusInternalServerError || hasSessionCookie(app, response) {
		t.Fatalf("login status=%d cookie=%v", response.StatusCode, hasSessionCookie(app, response))
	}
	if loggedIn(app) {
		t.Fatal("client holds a usable session after failed login")
	}
	var live int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM admin_sessions WHERE revoked_at IS NULL AND data LIKE '%administrator_id%'`).Scan(&live)
	if live != 0 {
		t.Fatalf("live authenticated sessions = %d, want 0 (compensation must revoke)", live)
	}
	if auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 0 || auditCount(t, app, domain.ActionLogin, domain.AuditFailed) != 1 {
		t.Fatalf("login audits succeeded=%d failed=%d", auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded), auditCount(t, app, domain.ActionLogin, domain.AuditFailed))
	}
	app.Login()
	if !loggedIn(app) || auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 1 {
		t.Fatal("subsequent login did not establish exactly one audited session")
	}
	assertNoCredentialLeak(t, app)
}

// T148：每次成功登录/登出的响应都伴随恰好一条 succeeded 审计，且会话可用性与之一致（协议可判定）。
func TestLoginLogoutProtocolIsDecidable(t *testing.T) {
	app, _, _ := newFaultyAuthApp(t)
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}})
	if response.StatusCode != http.StatusSeeOther || !hasSessionCookie(app, response) || !loggedIn(app) {
		t.Fatalf("login status=%d cookie=%v", response.StatusCode, hasSessionCookie(app, response))
	}
	if auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 1 {
		t.Fatal("successful login must be audited exactly once")
	}
	response, _ = app.PostForm("/logout", "/", nil)
	if response.StatusCode != http.StatusSeeOther || loggedIn(app) || auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 1 {
		t.Fatalf("logout status=%d loggedIn=%v", response.StatusCode, loggedIn(app))
	}
}
