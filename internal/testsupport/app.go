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
	Templates   *application.TemplateService
	Users       *application.UserService
	Connections *application.ConnectionService
	Settings    *application.SettingsService
	Traffic     *application.TrafficService
	Quota       *application.QuotaService
	Dashboard   *application.DashboardService
	Reconcile   *application.ReconciliationService
	Audit       *application.AuditService
	Sync        *worker.Synchronizer
	Validator   *worker.TemplateValidator
	Node        *sync.Mutex
	Sessions    *scs.SessionManager
	Handler     http.Handler
	Server      *httptest.Server
	Client      *http.Client
	Username    string
	Password    string
	Target      ports.InstanceTarget
	AdminID     domain.ID
}

const (
	DefaultUsername      = "admin"
	DefaultPassword      = "correct horse battery staple"
	DefaultListenAddress = "127.0.0.1"
	DefaultPoolStart     = 30000
	DefaultPoolEnd       = 30099
)

// Options 允许契约测试注入真实 Xray Adapter 与目标；零值等同 New（fake Adapter）。
type Options struct {
	Adapter ports.Adapter
	Target  *ports.InstanceTarget
	// WrapAuthStore / WrapSessionStore 允许测试包装认证存储与会话存储以注入故障。
	WrapAuthStore    func(ports.AuthStore) ports.AuthStore
	WrapSessionStore func(scs.Store) scs.Store
}

// New 装配应用并初始化管理员；不登录。
func New(t *testing.T) *App { return NewWith(t, Options{}) }

// NewWith 按 Options 装配应用：注入真实 Adapter 时 App.Adapter 为 nil，fake 专用助手不可用。
func NewWith(t *testing.T, options Options) *App {
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
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: config.SupportedXrayVersion, RPCTimeout: time.Second}
	if options.Target != nil {
		target = *options.Target
	}
	if err := store.EnsureSingletons(ctx, "UTC", target.APIEndpoint, config.SupportedXrayVersion, verifier, nonce, clock.Now()); err != nil {
		t.Fatal(err)
	}
	var adapter ports.Adapter
	var fakeAdapter *fake.Adapter
	if options.Adapter != nil {
		adapter = options.Adapter
	} else {
		fakeAdapter = fake.New()
		fakeAdapter.Now = clock.Now
		fakeAdapter.BootEpoch = clock.Now().Add(-time.Hour)
		adapter = fakeAdapter
	}
	node := &sync.Mutex{}
	app := &App{T: t, Store: store, Keyring: keyring, Clock: clock, Adapter: fakeAdapter, Node: node, Target: target,
		Username: DefaultUsername, Password: DefaultPassword}

	var authStore ports.AuthStore = store
	if options.WrapAuthStore != nil {
		authStore = options.WrapAuthStore(store)
	}
	app.Auth, err = application.NewAuthService(authStore, clock)
	if err != nil {
		t.Fatal(err)
	}
	app.AdminID, err = app.Auth.InitializeAdministrator(ctx, app.Username, []byte(app.Password))
	if err != nil {
		t.Fatal(err)
	}
	app.Templates = application.NewTemplateService(store, adapter, keyring, clock, target, func(id domain.ID) {
		if app.Validator != nil {
			app.Validator.Enqueue(id)
		}
	})
	app.Sync = worker.NewSynchronizer(store, adapter, keyring, clock, nil, node, worker.SynchronizerOptions{
		MaxRetryInterval: 30 * time.Second, LeaseDuration: 10 * time.Second, RPCTimeout: target.RPCTimeout, Random: func(n int64) int64 { return n - 1 }})
	app.Users = application.NewUserService(store, keyring, clock, app.Sync.Wake)
	app.Connections = application.NewConnectionService(store, keyring)
	app.Settings = application.NewSettingsService(store).WithClock(clock)
	app.Validator = worker.NewTemplateValidator(app.Templates, store, nil, node, 15*time.Second)
	app.Traffic = application.NewTrafficService(store, adapter, clock, target, 5*time.Second, app.Sync.Wake, nil)
	app.Quota = application.NewQuotaService(store, clock, app.Sync.Wake, nil)
	app.Dashboard = application.NewDashboardService(store, clock, 5*time.Second, 15*time.Second)
	app.Reconcile = application.NewReconciliationService(store, adapter, clock, target, node, 15*time.Second, app.Sync.Wake, app.Templates.RunValidation, nil)
	app.Audit = application.NewAuditService(store)

	app.Sessions = scs.New()
	var sessionStore scs.Store = sqlite.NewSessionStore(db, 30*time.Minute, 12*time.Hour)
	if options.WrapSessionStore != nil {
		sessionStore = options.WrapSessionStore(sessionStore)
	}
	webmiddleware.ConfigureSessions(app.Sessions, sessionStore, 30*time.Minute, 12*time.Hour, false)
	app.Handler, err = web.Routes(web.RouteDependencies{Auth: app.Auth, Templates: app.Templates, Users: app.Users,
		Connections: app.Connections, Settings: app.Settings, Dashboard: app.Dashboard, Audit: app.Audit, Sessions: app.Sessions, CSRFKey: keyring.CSRFKey(), Secure: false,
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

// RegisterCompatibleTemplate 通过服务层登记入站模板并同步验证为 compatible。
func (a *App) RegisterCompatibleTemplate(name string) domain.ID {
	a.T.Helper()
	return a.RegisterTemplateWithPool(name, DefaultPoolStart, DefaultPoolEnd)
}

// RegisterTemplateWithPool 登记一个指定端口池区间的兼容入站模板，用于端口分配与池耗尽测试。
func (a *App) RegisterTemplateWithPool(name string, poolStart, poolEnd int) domain.ID {
	a.T.Helper()
	id, err := a.Templates.RegisterTemplate(context.Background(), application.TemplateInput{Name: name,
		PublicHost: "vpn.example.com", ListenAddress: DefaultListenAddress, PortPoolStart: poolStart,
		PortPoolEnd: poolEnd, Method: security.MethodAES256, Network: domain.NetworkTCPUDP,
		RequestID: NewID(a.T), ActorID: NewID(a.T)})
	if err != nil {
		a.T.Fatal(err)
	}
	if err := a.Validator.ValidateNow(context.Background(), id); err != nil {
		a.T.Fatal(err)
	}
	record, err := a.Store.Template(context.Background(), id)
	if err != nil || record.Template.Compatibility != domain.CompatibilityCompatible {
		a.T.Fatalf("template = %#v, %v", record.Template, err)
	}
	return id
}

// Listening 判定某端口当前是否被 fake Xray 中的面板入站占用。
func (a *App) Listening(port int) bool {
	a.T.Helper()
	if a.Adapter == nil {
		a.T.Fatal("Listening requires the fake adapter")
	}
	return a.Adapter.Listening(port)
}

// CreateUser 通过服务层创建用户，返回完整记录（未同步）。
func (a *App) CreateUser(name string, templateID domain.ID, limit *int64) ports.UserRecord {
	a.T.Helper()
	id, _, err := a.Users.CreateUser(context.Background(), application.CreateUserInput{DisplayName: name, TemplateID: templateID,
		LimitBytes: limit, ResetDay: 1, RequestID: NewID(a.T), ActorID: NewID(a.T)})
	if err != nil {
		a.T.Fatal(err)
	}
	return a.User(id)
}

// ListUsers 返回全部未删除用户，按创建顺序，供端口分配等测试核对总量。
func (a *App) ListUsers() []ports.UserRecord {
	a.T.Helper()
	records, err := a.Store.ListUsers(context.Background(), ports.UserFilter{})
	if err != nil {
		a.T.Fatal(err)
	}
	return records
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

// Collect 执行一轮采集并断言成功。
func (a *App) Collect() application.CollectionSummary {
	a.T.Helper()
	summary, err := a.Traffic.CollectOnce(context.Background())
	if err != nil {
		a.T.Fatal(err)
	}
	return summary
}

// Rollover 结算到期周期，返回切换次数。
func (a *App) Rollover() int {
	a.T.Helper()
	count, err := a.Quota.RolloverDue(context.Background(), a.Clock.Now())
	if err != nil {
		a.T.Fatal(err)
	}
	return count
}

// SetTraffic 设置 fake Xray 中某分配的绝对计数。
func (a *App) SetTraffic(record ports.UserRecord, uplink, downlink uint64) {
	a.Adapter.SetCounter(record.Identity.StatisticsID, ports.Uplink, uplink)
	a.Adapter.SetCounter(record.Identity.StatisticsID, ports.Downlink, downlink)
}

// ReconcileOnce 执行一轮协调并断言成功。
func (a *App) ReconcileOnce() application.ReconcileSummary {
	a.T.Helper()
	summary, err := a.Reconcile.ReconcileOnce(context.Background())
	if err != nil {
		a.T.Fatal(err)
	}
	return summary
}
