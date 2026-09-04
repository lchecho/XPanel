package testsupport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/config"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/web"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/worker"
)

// App 是一个完整装配的 XPanel 实例：真实 SQLite、fake Xray Adapter、固定时钟、全部 application service、
// worker 与 HTTP 路由；worker 不自动运行，测试通过 Drain/ValidateNow 显式推进。
type App struct {
	T           *testing.T
	Store       *sqlite.Store
	Keyring     *security.Keyring
	Clock       *ports.FixedClock
	Adapter     *fake.Adapter
	Auth        *application.AuthService
	Profiles    *application.ProfileService
	Users       *application.UserService
	Connections *application.ConnectionService
	Settings    *application.SettingsService
	Sync        *worker.Synchronizer
	Validator   *worker.ProfileValidator
	Node        *sync.Mutex
	Sessions    *scs.SessionManager
	Handler     http.Handler
	Server      *httptest.Server
	Client      *http.Client
	Username    string
	Password    string
	Target      ports.InstanceTarget
}

const (
	DefaultUsername = "admin"
	DefaultPassword = "correct horse battery staple"
	ProfileTag      = "managed"
	BootstrapID     = "bootstrap"
)

// New 装配应用并初始化管理员；不登录。
func New(t *testing.T) *App {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ctx, filepath.Join(dir, "xpanel.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	clock := &ports.FixedClock{Time: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	verifier, nonce, err := keyring.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSingletons(ctx, "UTC", "127.0.0.1:10085", config.SupportedXrayVersion, verifier, nonce, clock.Now()); err != nil {
		t.Fatal(err)
	}
	adapter := fake.New()
	adapter.Now = clock.Now
	adapter.BootEpoch = clock.Now().Add(-time.Hour)
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: config.SupportedXrayVersion, RPCTimeout: time.Second}
	node := &sync.Mutex{}
	app := &App{T: t, Store: store, Keyring: keyring, Clock: clock, Adapter: adapter, Node: node, Target: target,
		Username: DefaultUsername, Password: DefaultPassword}

	app.Auth, err = application.NewAuthService(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Auth.InitializeAdministrator(ctx, app.Username, []byte(app.Password)); err != nil {
		t.Fatal(err)
	}
	app.Profiles = application.NewProfileService(store, adapter, keyring, clock, target, func(id domain.ID) {
		if app.Validator != nil {
			app.Validator.Enqueue(id)
		}
	})
	app.Sync = worker.NewSynchronizer(store, adapter, keyring, clock, nil, node, worker.SynchronizerOptions{
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, Random: func(n int64) int64 { return n - 1 }})
	app.Users = application.NewUserService(store, keyring, clock, app.Sync.Wake)
	app.Connections = application.NewConnectionService(store, keyring)
	app.Settings = application.NewSettingsService(store)
	app.Validator = worker.NewProfileValidator(app.Profiles, store, nil, node, 15*time.Second)

	app.Sessions = scs.New()
	webmiddleware.ConfigureSessions(app.Sessions, sqlite.NewSessionStore(db, 30*time.Minute, 12*time.Hour), 30*time.Minute, 12*time.Hour, false)
	app.Handler, err = web.Routes(web.RouteDependencies{Auth: app.Auth, Profiles: app.Profiles, Users: app.Users,
		Connections: app.Connections, Settings: app.Settings, Sessions: app.Sessions, CSRFKey: keyring.CSRFKey(), Secure: false,
		Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	app.Server = httptest.NewServer(app.Handler)
	jar, _ := cookiejar.New(nil)
	app.Client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() { app.Server.Close(); _ = store.Close() })
	return app
}

var csrfPattern = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// Get 发起 GET 并返回响应与正文。
func (a *App) Get(path string) (*http.Response, string) {
	a.T.Helper()
	response, err := a.Client.Get(a.Server.URL + path)
	if err != nil {
		a.T.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		a.T.Fatal(err)
	}
	return response, string(body)
}

// CSRF 从任意含表单的页面提取当前会话的 CSRF 令牌。
func (a *App) CSRF(path string) string {
	a.T.Helper()
	_, body := a.Get(path)
	match := csrfPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		a.T.Fatalf("CSRF field not found in %s", path)
	}
	return match[1]
}

// PostForm 提交表单；自动补齐 _csrf（取自 tokenPath）与 _request_id（缺省新生成）。
func (a *App) PostForm(path, tokenPath string, form url.Values) (*http.Response, string) {
	a.T.Helper()
	if form == nil {
		form = url.Values{}
	}
	if form.Get("_csrf") == "" {
		form.Set("_csrf", a.CSRF(tokenPath))
	}
	if form.Get("_request_id") == "" {
		form.Set("_request_id", NewID(a.T).String())
	}
	request, err := http.NewRequest(http.MethodPost, a.Server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		a.T.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.Client.Do(request)
	if err != nil {
		a.T.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		a.T.Fatal(err)
	}
	return response, string(body)
}

// Login 以默认管理员登录并断言成功。
func (a *App) Login() {
	a.T.Helper()
	response, body := a.PostForm("/login", "/login", url.Values{"username": {a.Username}, "password": {a.Password}})
	if response.StatusCode != http.StatusSeeOther {
		a.T.Fatalf("login status %d: %s", response.StatusCode, body)
	}
}

// RegisterCompatibleProfile 通过服务层登记 profile 并同步验证为 compatible。
func (a *App) RegisterCompatibleProfile(name string) domain.ID {
	a.T.Helper()
	key, err := security.GenerateUserKey(security.MethodAES256)
	if err != nil {
		a.T.Fatal(err)
	}
	id, err := a.Profiles.RegisterProfile(context.Background(), application.ProfileInput{Name: name, InboundTag: ProfileTag,
		PublicHost: "vpn.example.com", PublicPort: 8388, Method: security.MethodAES256, Network: domain.NetworkTCPUDP,
		ServerKey: key.Reveal(), BootstrapStatisticsID: BootstrapID, RequestID: NewID(a.T), ActorID: NewID(a.T)})
	if err != nil {
		a.T.Fatal(err)
	}
	if err := a.Validator.ValidateNow(context.Background(), id); err != nil {
		a.T.Fatal(err)
	}
	record, err := a.Store.Profile(context.Background(), id)
	if err != nil || record.Profile.Compatibility != domain.CompatibilityCompatible {
		a.T.Fatalf("profile = %#v, %v", record.Profile, err)
	}
	return id
}

// CreateUser 通过服务层创建用户，返回完整记录（未同步）。
func (a *App) CreateUser(name string, profileID domain.ID, limit *int64) ports.UserRecord {
	a.T.Helper()
	id, _, err := a.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: name, ProfileID: profileID,
		LimitBytes: limit, ResetDay: 1, RequestID: NewID(a.T), ActorID: NewID(a.T)})
	if err != nil {
		a.T.Fatal(err)
	}
	return a.User(id)
}

func (a *App) User(id domain.ID) ports.UserRecord {
	a.T.Helper()
	record, err := a.Store.User(context.Background(), id)
	if err != nil {
		a.T.Fatal(err)
	}
	return record
}

// Drain 让同步 worker 处理全部到期操作。
func (a *App) Drain() int {
	a.T.Helper()
	processed, err := a.Sync.Drain(context.Background())
	if err != nil {
		a.T.Fatal(err)
	}
	return processed
}

func NewID(t *testing.T) domain.ID {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
