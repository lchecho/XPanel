package integration

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"xpanel/internal/application"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/web"
	webmiddleware "xpanel/internal/web/middleware"
)

func TestAuthenticationCSRFAndSecurityHeaders(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "xpanel.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(context.Background(), db.Write); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	root := make([]byte, 32)
	_, _ = rand.Read(root)
	keyring, _ := security.NewKeyring(root)
	verifier, nonce, _ := keyring.NewVerifier()
	if err := store.EnsureSingletons(context.Background(), "UTC", "127.0.0.1:10085", "v26.3.27", verifier, nonce, time.Now()); err != nil {
		t.Fatal(err)
	}
	auth, err := application.NewAuthService(store, ports.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.InitializeAdministrator(context.Background(), "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	sessions := scs.New()
	webmiddleware.ConfigureSessions(sessions, sqlite.NewSessionStore(db, 30*time.Minute, 12*time.Hour), 30*time.Minute, 12*time.Hour, false)
	handler, err := web.Routes(web.RouteDependencies{Auth: auth, Sessions: sessions, CSRFKey: keyring.CSRFKey(), Secure: false, Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	response, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated status = %d", response.StatusCode)
	}
	response, err = client.Get(server.URL + "/fragments/users-table")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("fragment status = %d", response.StatusCode)
	}

	response, err = client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	token := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindSubmatch(body)
	if len(token) != 2 {
		t.Fatalf("missing CSRF token: %s", body)
	}
	badForm := url.Values{"_csrf": {string(token[1])}, "_request_id": {"550e8400-e29b-41d4-a716-446655440000"},
		"username": {"admin"}, "password": {"super-secret-wrong-password"}}
	badRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/login", strings.NewReader(badForm.Encode()))
	badRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badResponse, err := client.Do(badRequest)
	if err != nil {
		t.Fatal(err)
	}
	badBody, _ := io.ReadAll(badResponse.Body)
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusUnauthorized || strings.Contains(string(badBody), "super-secret-wrong-password") {
		t.Fatalf("unsafe login failure status=%d body=%s", badResponse.StatusCode, badBody)
	}
	response, err = client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	token = regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindSubmatch(body)
	form := url.Values{"_csrf": {string(token[1])}, "_request_id": {"550e8400-e29b-41d4-a716-446655440000"},
		"username": {"admin"}, "password": {"correct horse battery staple"}}
	crossSiteRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/login", strings.NewReader(form.Encode()))
	crossSiteRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossSiteRequest.Header.Set("Origin", "null")
	crossSiteRequest.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteResponse, err := client.Do(crossSiteRequest)
	if err != nil {
		t.Fatal(err)
	}
	crossSiteResponse.Body.Close()
	if crossSiteResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site null-origin login status = %d", crossSiteResponse.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Referrer-Policy: no-referrer makes Chrome serialize Origin as "null" for
	// a normal form POST. Fetch Metadata still proves this navigation is same-origin.
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", response.StatusCode)
	}

	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	dashboard, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d body=%s", response.StatusCode, dashboard)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("security headers missing")
	}
	dashboardToken := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindSubmatch(dashboard)
	if len(dashboardToken) != 2 {
		t.Fatalf("dashboard lacks logout CSRF field: %s", dashboard)
	}
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/logout", strings.NewReader("_request_id=550e8400-e29b-41d4-a716-446655440000"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF failure status = %d", response.StatusCode)
	}
	logoutForm := url.Values{"_csrf": {string(dashboardToken[1])}, "_request_id": {"550e8400-e29b-41d4-a716-446655440000"}}
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/logout", strings.NewReader(logoutForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d", response.StatusCode)
	}
	response, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoked session status = %d", response.StatusCode)
	}
}
