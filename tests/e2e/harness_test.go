package e2e

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

	"xpanel/internal/adapter/xray/fake"
	"xpanel/internal/application"
	"xpanel/internal/config"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
	"xpanel/internal/web"
	webmiddleware "xpanel/internal/web/middleware"
)

type harness struct {
	store    *sqlite.Store
	adapter  *fake.Adapter
	clock    *ports.FixedClock
	server   *httptest.Server
	client   *http.Client
	username string
	password string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "xpanel.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(context.Background(), db.Write); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	root := make([]byte, 32)
	if _, err := rand.Read(root); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.NewKeyring(root)
	if err != nil {
		t.Fatal(err)
	}
	verifier, nonce, err := keyring.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.EnsureSingletons(context.Background(), "UTC", "127.0.0.1:10085", config.SupportedXrayVersion, verifier, nonce, now); err != nil {
		t.Fatal(err)
	}
	clock := &ports.FixedClock{Time: now}
	auth, err := application.NewAuthService(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	username, password := "admin", "correct horse battery staple"
	if _, err := auth.InitializeAdministrator(context.Background(), username, []byte(password)); err != nil {
		t.Fatal(err)
	}
	sessions := scs.New()
	webmiddleware.ConfigureSessions(sessions, sqlite.NewSessionStore(db, 30*time.Minute, 12*time.Hour),
		30*time.Minute, 12*time.Hour, false)
	handler, err := web.Routes(web.RouteDependencies{Auth: auth, Sessions: sessions, CSRFKey: keyring.CSRFKey(), Secure: false, Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	h := &harness{store: store, adapter: fake.New(), clock: clock, server: server, client: client, username: username, password: password}
	t.Cleanup(func() { server.Close(); _ = store.Close() })
	return h
}

var csrfPattern = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func (h *harness) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	response, err := h.client.Get(h.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	match := csrfPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("CSRF field not found in %q", body)
	}
	return match[1]
}

func (h *harness) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := h.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (h *harness) login(t *testing.T) {
	_, body := h.get(t, "/login")
	response := h.post(t, "/login", url.Values{"_csrf": {csrfFrom(t, body)}, "_request_id": {newTestID()},
		"username": {h.username}, "password": {h.password}})
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("login status %d: %s", response.StatusCode, data)
	}
}

func newTestID() string {
	id, _ := domain.NewID()
	return id.String()
}
