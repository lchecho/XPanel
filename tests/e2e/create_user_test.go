package e2e

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"xpanel/internal/domain"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

func registerProfileViaBrowser(t *testing.T, app *testsupport.App, name, tag string) (domain.ID, string) {
	t.Helper()
	key, err := security.GenerateUserKey(security.MethodAES256)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {name}, "inbound_tag": {tag}, "public_host": {"vpn.example.com"}, "public_port": {"8388"},
		"method": {security.MethodAES256}, "network": {"tcp_udp"}, "server_key": {key.Reveal()}, "bootstrap_statistics_id": {"bootstrap"}}
	response, body := app.PostForm("/profiles", "/profiles/new", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("register profile status=%d body=%s", response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	return domain.ID(strings.TrimPrefix(location, "/profiles/")), location
}

func TestCreateUserEndToEnd(t *testing.T) {
	app := newHarness(t)
	app.Login()
	profileID, profilePath := registerProfileViaBrowser(t, app, "Primary", "managed")
	if err := app.Validator.ValidateNow(context.Background(), profileID); err != nil {
		t.Fatal(err)
	}
	if _, body := app.Get(profilePath); !strings.Contains(body, "兼容") {
		t.Fatalf("profile not compatible: %s", body)
	}
	response, body := app.PostForm("/users", "/users/new", url.Values{"display_name": {"Alice"}, "profile_id": {profileID.String()},
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
	if _, ok := app.Adapter.Users[testsupport.ProfileTag][record.Identity.StatisticsID]; !ok {
		t.Fatal("fake Xray does not contain the new user")
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

func TestCreateUserRejectsIncompatibleProfile(t *testing.T) {
	app := newHarness(t)
	app.Login()
	app.Adapter.Profiles["single"] = ports.ProfileCapabilities{InboundPresent: true, ProtocolSupported: true, MethodSupported: true,
		CompatibilityReason: "inbound has no bootstrap user; single-user mode"}
	profileID, profilePath := registerProfileViaBrowser(t, app, "Single", "single")
	if err := app.Validator.ValidateNow(context.Background(), profileID); err != nil {
		t.Fatal(err)
	}
	_, body := app.Get(profilePath)
	if !strings.Contains(body, "不兼容") || !strings.Contains(body, "等待运维修复") {
		t.Fatalf("incompatible profile page body=%s", body)
	}
	_, body = app.Get("/users/new")
	if !strings.Contains(body, "当前没有处于“兼容”状态的访问配置") {
		t.Fatalf("incompatible profile offered for creation: %s", body)
	}
	response, body := app.PostForm("/users", "/profiles", url.Values{"display_name": {"Alice"}, "profile_id": {profileID.String()},
		"unlimited": {"on"}, "reset_day": {"1"}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "所选访问配置当前不兼容") {
		t.Fatalf("incompatible create status=%d body=%s", response.StatusCode, body)
	}
}

func TestCreateUserWhileXrayOfflineConvergesLater(t *testing.T) {
	app := newHarness(t)
	app.Login()
	profileID := app.RegisterCompatibleProfile("Primary")
	app.Adapter.Available = false
	response, _ := app.PostForm("/users", "/users/new", url.Values{"display_name": {"Offline"}, "profile_id": {profileID.String()},
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
	profileID := app.RegisterCompatibleProfile("Primary")
	record := app.CreateUser("Alice", profileID, nil)
	app.Drain()
	anonymous := newHarness(t)
	for _, path := range []string{"/users", "/users/" + record.User.ID.String(), "/users/" + record.User.ID.String() + "/connection", "/profiles"} {
		response, body := anonymous.Get(path)
		if response.StatusCode != http.StatusSeeOther || strings.Contains(body, "ss://") || strings.Contains(body, "Alice") {
			t.Fatalf("%s status=%d body=%s", path, response.StatusCode, body)
		}
	}
}
