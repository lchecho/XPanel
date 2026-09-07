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

// faultAuthStore 让下一次认证写事务整体失败，或让事务内的指定语句（InsertSession/RevokeSession/AppendAudit）失败一次。
type faultAuthStore struct {
	ports.AuthStore
	mu       sync.Mutex
	failNext bool
	fails    map[string]bool
}

func (f *faultAuthStore) FailNextWrite() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = true
}

// FailNext 让事务内的下一条指定语句失败（事务随之整体回滚）。
func (f *faultAuthStore) FailNext(statement string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails == nil {
		f.fails = map[string]bool{}
	}
	f.fails[statement] = true
}

func (f *faultAuthStore) take(statement string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails[statement] {
		delete(f.fails, statement)
		return true
	}
	return false
}

func (f *faultAuthStore) WithWriteTx(ctx context.Context, fn func(ports.WriteTx) error) error {
	f.mu.Lock()
	fail := f.failNext
	f.failNext = false
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	return f.AuthStore.WithWriteTx(ctx, func(tx ports.WriteTx) error { return fn(&faultWriteTx{WriteTx: tx, store: f}) })
}

type faultWriteTx struct {
	ports.WriteTx
	store *faultAuthStore
}

func (t *faultWriteTx) InsertSession(ctx context.Context, record ports.SessionRecord) error {
	if t.store.take("InsertSession") {
		return errInjected
	}
	return t.WriteTx.InsertSession(ctx, record)
}

func (t *faultWriteTx) RevokeSession(ctx context.Context, digest []byte, now time.Time) (bool, error) {
	if t.store.take("RevokeSession") {
		return false, errInjected
	}
	return t.WriteTx.RevokeSession(ctx, digest, now)
}

func (t *faultWriteTx) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	if t.store.take("AppendAudit") {
		return errInjected
	}
	return t.WriteTx.AppendAudit(ctx, event)
}

// faultSessionStore 让下一次会话提交或撤销失败；同时实现 scs CtxStore，使登录/登出仍走事务内路径。
type faultSessionStore struct {
	scs.Store
	mu         sync.Mutex
	failCommit bool
	failDelete bool
}

func (f *faultSessionStore) FailNextCommit() { f.mu.Lock(); f.failCommit = true; f.mu.Unlock() }
func (f *faultSessionStore) FailNextDelete() { f.mu.Lock(); f.failDelete = true; f.mu.Unlock() }

func (f *faultSessionStore) takeCommit() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := f.failCommit
	f.failCommit = false
	return fail
}

func (f *faultSessionStore) takeDelete() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := f.failDelete
	f.failDelete = false
	return fail
}

func (f *faultSessionStore) Commit(token string, data []byte, expiry time.Time) error {
	if f.takeCommit() {
		return errInjected
	}
	return f.Store.Commit(token, data, expiry)
}

func (f *faultSessionStore) Delete(token string) error {
	if f.takeDelete() {
		return errInjected
	}
	return f.Store.Delete(token)
}

func (f *faultSessionStore) CommitCtx(ctx context.Context, token string, data []byte, expiry time.Time) error {
	if f.takeCommit() {
		return errInjected
	}
	return f.Store.(scs.CtxStore).CommitCtx(ctx, token, data, expiry)
}

func (f *faultSessionStore) DeleteCtx(ctx context.Context, token string) error {
	if f.takeDelete() {
		return errInjected
	}
	return f.Store.(scs.CtxStore).DeleteCtx(ctx, token)
}

func (f *faultSessionStore) FindCtx(ctx context.Context, token string) ([]byte, bool, error) {
	return f.Store.(scs.CtxStore).FindCtx(ctx, token)
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
	if live := liveAuthenticatedSessions(t, app); live != 0 {
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

// T147/T152：登出写事务整体失败——审计与撤销一起回滚：HTTP 不返回成功、原会话仍可用、没有 succeeded 审计。
func TestLogoutAuditFailureDoesNotReportSuccess(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	app.Login()
	auth.FailNextWrite()
	response, _ := app.PostForm("/logout", "/", nil)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("logout with audit failure status=%d, want 500", response.StatusCode)
	}
	if !loggedIn(app) || liveAuthenticatedSessions(t, app) != 1 {
		t.Fatal("session revoked although the logout transaction rolled back")
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 0 || auditCount(t, app, domain.ActionLogout, domain.AuditFailed) != 1 {
		t.Fatalf("logout audits succeeded=%d failed=%d", auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded), auditCount(t, app, domain.ActionLogout, domain.AuditFailed))
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

// T153：登录后的普通请求由中间件做辅助会话刷新（idle 滑动 + flash）；刷新失败不得把已提交的业务成功改报 500：
// 设置只提交一次、HTTP 状态与事实一致、不写新会话 cookie、原会话保持可用，同 request ID 重放不重复变更。
func TestMiddlewareSessionCommitFailureLeavesNoPartialState(t *testing.T) {
	app, _, sessions := newFaultyAuthApp(t)
	app.Login()
	settings, _ := app.Store.Settings(context.Background())
	auditsBefore := auditCount(t, app, domain.ActionSettingsUpdated, domain.AuditSucceeded)
	// scs 在配置了 idle timeout 时每次加载都会重新提交会话（滑动过期），因此先取 CSRF 令牌再注入提交故障。
	csrf := app.CSRF("/settings")
	requestID := testsupport.NewID(t).String()
	form := url.Values{"quota_timezone": {"Asia/Tokyo"}, "_version": {fmt.Sprint(settings.Revision)}, "_csrf": {csrf}, "_request_id": {requestID}}
	sessions.FailNextCommit()
	response, _ := app.PostForm("/settings", "/settings", form)
	if response.StatusCode != http.StatusSeeOther || hasSessionCookie(app, response) {
		t.Fatalf("auxiliary refresh failure: status=%d cookie=%v (business succeeded, response must say so without a new cookie)",
			response.StatusCode, hasSessionCookie(app, response))
	}
	if !loggedIn(app) {
		t.Fatal("previously established session lost after a failed auxiliary refresh")
	}
	after, _ := app.Store.Settings(context.Background())
	if after.QuotaTimezone != "Asia/Tokyo" || after.Revision != settings.Revision+1 {
		t.Fatalf("settings after request = %#v (want exactly one committed change)", after)
	}
	if auditCount(t, app, domain.ActionSettingsUpdated, domain.AuditSucceeded) != auditsBefore+1 {
		t.Fatal("business fact audited other than exactly once")
	}
	// 同一 request ID 与载荷重放：幂等，不重复变更。
	form.Set("_csrf", app.CSRF("/settings"))
	response, _ = app.PostForm("/settings", "/settings", form)
	replayed, _ := app.Store.Settings(context.Background())
	if response.StatusCode != http.StatusSeeOther || replayed.Revision != settings.Revision+1 ||
		auditCount(t, app, domain.ActionSettingsUpdated, domain.AuditSucceeded) != auditsBefore+1 {
		t.Fatalf("replay status=%d revision=%d (want idempotent replay)", response.StatusCode, replayed.Revision)
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
	if live := liveAuthenticatedSessions(t, app); live != 0 {
		t.Fatalf("live authenticated sessions = %d, want 0 (nothing may be persisted after a rolled-back login)", live)
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

func liveAuthenticatedSessions(t *testing.T, app *testsupport.App) int {
	t.Helper()
	// 会话数据是 gob 编码的 BLOB，不能用 LIKE 判断内容；这些测试里所有会话行都是登录建立的认证会话。
	var live int
	if err := app.Store.DB().Read.QueryRow(`SELECT count(*) FROM admin_sessions WHERE revoked_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	return live
}

// T152：会话行插入失败——事务整体回滚：无 live session、无 succeeded 审计、只有独立事务写入的 failed 审计。
func TestLoginSessionInsertFailureRollsBackAtomically(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	auth.FailNext("InsertSession")
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}})
	if response.StatusCode != http.StatusInternalServerError || hasSessionCookie(app, response) || loggedIn(app) {
		t.Fatalf("login with session insert failure: status=%d cookie=%v loggedIn=%v", response.StatusCode, hasSessionCookie(app, response), loggedIn(app))
	}
	if liveAuthenticatedSessions(t, app) != 0 || auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 0 || auditCount(t, app, domain.ActionLogin, domain.AuditFailed) != 1 {
		t.Fatalf("live=%d succeeded=%d failed=%d", liveAuthenticatedSessions(t, app), auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded), auditCount(t, app, domain.ActionLogin, domain.AuditFailed))
	}
	app.Login()
	if !loggedIn(app) || auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 1 {
		t.Fatal("retry did not establish exactly one audited session")
	}
}

// T152：登录 succeeded 审计语句失败——会话插入随事务回滚，浏览器不得拿到 cookie。
func TestLoginAuditStatementFailureRollsBackSession(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	auth.FailNext("AppendAudit")
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {app.Username}, "password": {app.Password}})
	if response.StatusCode != http.StatusInternalServerError || hasSessionCookie(app, response) || loggedIn(app) {
		t.Fatalf("login with audit statement failure: status=%d cookie=%v loggedIn=%v", response.StatusCode, hasSessionCookie(app, response), loggedIn(app))
	}
	if liveAuthenticatedSessions(t, app) != 0 || auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded) != 0 || auditCount(t, app, domain.ActionLogin, domain.AuditFailed) != 1 {
		t.Fatalf("live=%d succeeded=%d failed=%d", liveAuthenticatedSessions(t, app), auditCount(t, app, domain.ActionLogin, domain.AuditSucceeded), auditCount(t, app, domain.ActionLogin, domain.AuditFailed))
	}
	assertNoCredentialLeak(t, app)
}

// T152：登出撤销语句失败——审计随事务回滚，原会话仍可用，只有 failed 审计；重试后恰好一个 succeeded。
func TestLogoutRevokeStatementFailureKeepsSessionUsable(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	app.Login()
	auth.FailNext("RevokeSession")
	response, _ := app.PostForm("/logout", "/", nil)
	if response.StatusCode != http.StatusInternalServerError || !loggedIn(app) {
		t.Fatalf("logout with revoke failure: status=%d loggedIn=%v", response.StatusCode, loggedIn(app))
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 0 || auditCount(t, app, domain.ActionLogout, domain.AuditFailed) != 1 || liveAuthenticatedSessions(t, app) != 1 {
		t.Fatalf("succeeded=%d failed=%d live=%d", auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded), auditCount(t, app, domain.ActionLogout, domain.AuditFailed), liveAuthenticatedSessions(t, app))
	}
	if response, _ = app.PostForm("/logout", "/", nil); response.StatusCode != http.StatusSeeOther || loggedIn(app) {
		t.Fatalf("retry logout status=%d loggedIn=%v", response.StatusCode, loggedIn(app))
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 1 || liveAuthenticatedSessions(t, app) != 0 {
		t.Fatal("retry must leave exactly one succeeded logout audit and no live session")
	}
}

// T152：登出 succeeded 审计语句失败——撤销随事务回滚，原会话仍可用；重试后恰好一个 succeeded。
func TestLogoutAuditStatementFailureKeepsSessionUsable(t *testing.T) {
	app, auth, _ := newFaultyAuthApp(t)
	app.Login()
	auth.FailNext("AppendAudit")
	response, _ := app.PostForm("/logout", "/", nil)
	if response.StatusCode != http.StatusInternalServerError || !loggedIn(app) || liveAuthenticatedSessions(t, app) != 1 {
		t.Fatalf("logout with audit failure: status=%d loggedIn=%v live=%d", response.StatusCode, loggedIn(app), liveAuthenticatedSessions(t, app))
	}
	if auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 0 || auditCount(t, app, domain.ActionLogout, domain.AuditFailed) != 1 {
		t.Fatalf("succeeded=%d failed=%d", auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded), auditCount(t, app, domain.ActionLogout, domain.AuditFailed))
	}
	if response, _ = app.PostForm("/logout", "/", nil); response.StatusCode != http.StatusSeeOther || loggedIn(app) || auditCount(t, app, domain.ActionLogout, domain.AuditSucceeded) != 1 {
		t.Fatalf("retry logout status=%d loggedIn=%v", response.StatusCode, loggedIn(app))
	}
}
