package e2e

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

// registerTemplateViaBrowser 通过浏览器表单登记入站模板：只有监听地址与端口池，没有入站标签与服务端密钥（FR-004）。
func registerTemplateViaBrowser(t *testing.T, app *testsupport.App, name string) (domain.ID, string) {
	t.Helper()
	form := url.Values{"name": {name}, "public_host": {"vpn.example.com"}, "listen_address": {testsupport.DefaultListenAddress},
		"port_pool_start": {strconv.Itoa(testsupport.DefaultPoolStart)}, "port_pool_end": {strconv.Itoa(testsupport.DefaultPoolEnd)},
		"method": {security.MethodAES256}, "network": {"tcp_udp"}}
	response, body := app.PostForm("/templates", "/templates/new", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("register template status=%d body=%s", response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	return domain.ID(strings.TrimPrefix(location, "/templates/")), location
}

func TestCreateUserEndToEnd(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID, templatePath := registerTemplateViaBrowser(t, app, "Primary")
	if err := app.Validator.ValidateNow(context.Background(), templateID); err != nil {
		t.Fatal(err)
	}
	if _, body := app.Get(templatePath); !strings.Contains(body, "兼容") {
		t.Fatalf("template not compatible: %s", body)
	}
	response, body := app.PostForm("/users", "/users/new", url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()},
		"quota_value": {"10"}, "quota_unit": {"GiB"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("create user status=%d body=%s", response.StatusCode, body)
	}
	userPath := response.Header.Get("Location")
	userID := domain.ID(strings.TrimPrefix(userPath, "/users/"))
	if app.Drain() != 1 {
		t.Fatal("synchronizer did not process the create operation")
	}
	record := app.User(userID)
	// 用户拥有一条专属入站：标签在面板命名空间内、端口取自模板端口池、入站内仅此一个受管客户端。
	tag := record.Inbound.Inbound.InboundTag
	if _, ok := app.Adapter.Inbounds[tag]; !ok {
		t.Fatalf("fake Xray does not contain the dedicated inbound %s", tag)
	}
	if _, ok := app.Adapter.Users[tag][record.Identity.StatisticsID]; !ok {
		t.Fatal("dedicated inbound does not contain the managed client")
	}
	if got := record.Inbound.Inbound.Port; got != testsupport.DefaultPoolStart {
		t.Fatalf("assigned port = %d want %d", got, testsupport.DefaultPoolStart)
	}
	if !app.Listening(record.Inbound.Inbound.Port) {
		t.Fatalf("port %d is not listening", record.Inbound.Inbound.Port)
	}
	if record.Allocation.ProjectionState != domain.ProjectionPresent || record.Credential.State != domain.CredentialActive {
		t.Fatalf("record after sync = %#v", record.Allocation)
	}
	_, body = app.Get(userPath + "/connection")
	if !strings.Contains(body, "ss://") || !strings.Contains(body, security.MethodAES256) {
		t.Fatalf("connection page body=%s", body)
	}
	var audits int
	if err := app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE target_id=? AND action IN (?,?)`,
		userID.String(), domain.ActionUserCreated, domain.ActionSyncSucceeded).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit events = %d, %v", audits, err)
	}
}

func TestCreateUserRejectsIncompatibleTemplate(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID, templatePath := registerTemplateViaBrowser(t, app, "Single")
	app.Adapter.Templates[templateID.String()] = ports.TemplateCapabilities{InboundCreatable: true, ProtocolSupported: true,
		MethodSupported: true, MultiUserSupported: false, CompatibilityReason: "node does not satisfy the SS2022 multi-user contract"}
	if err := app.Validator.ValidateNow(context.Background(), templateID); err != nil {
		t.Fatal(err)
	}
	_, body := app.Get(templatePath)
	if !strings.Contains(body, "不兼容") || !strings.Contains(body, "等待运维修复") {
		t.Fatalf("incompatible template page body=%s", body)
	}
	_, body = app.Get("/users/new")
	if !strings.Contains(body, "当前没有处于“兼容”状态的入站模板") {
		t.Fatalf("incompatible template offered for creation: %s", body)
	}
	response, body := app.PostForm("/users", "/templates", url.Values{"display_name": {"Alice"}, "template_id": {templateID.String()},
		"unlimited": {"on"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "所选入站模板当前不兼容") {
		t.Fatalf("incompatible create status=%d body=%s", response.StatusCode, body)
	}
}

func TestCreateUserWhileXrayOfflineConvergesLater(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	app.Adapter.Available = false
	response, _ := app.PostForm("/users", "/users/new", url.Values{"display_name": {"Offline"}, "template_id": {templateID.String()},
		"unlimited": {"on"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("offline create status=%d", response.StatusCode)
	}
	userPath := response.Header.Get("Location")
	userID := domain.ID(strings.TrimPrefix(userPath, "/users/"))
	app.Drain()
	_, body := app.Get(userPath)
	if !strings.Contains(body, "启用中（待同步）") || !strings.Contains(body, "节点暂时不可达") {
		t.Fatalf("offline detail body=%s", body)
	}
	instance, _ := app.Store.ManagedInstance(context.Background())
	if instance.HealthState != "unreachable" {
		t.Fatalf("instance health = %s", instance.HealthState)
	}
	app.Adapter.Available = true
	app.Clock.Advance(31 * time.Second)
	if app.Drain() != 1 {
		t.Fatal("retry did not run after backoff")
	}
	record := app.User(userID)
	if record.Allocation.ProjectionState != domain.ProjectionPresent {
		t.Fatalf("projection after recovery = %s", record.Allocation.ProjectionState)
	}
	_, body = app.Get(userPath)
	if !strings.Contains(body, "已启用") || strings.Contains(body, "待同步") {
		t.Fatalf("recovered detail body=%s", body)
	}
}

func TestUnauthenticatedAccessIsDeniedWithoutLeakingConnectionInfo(t *testing.T) {
	app := newHarness(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	anonymous := newHarness(t)
	for _, path := range []string{"/users", "/users/" + record.User.ID.String(), "/users/" + record.User.ID.String() + "/connection", "/templates"} {
		response, body := anonymous.Get(path)
		if response.StatusCode != http.StatusSeeOther || strings.Contains(body, "ss://") || strings.Contains(body, "Alice") {
			t.Fatalf("%s status=%d body=%s", path, response.StatusCode, body)
		}
	}
}
