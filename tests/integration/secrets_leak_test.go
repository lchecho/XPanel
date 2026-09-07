package integration

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/logging"
	"xpanel/internal/security"
	"xpanel/internal/testsupport"
)

// 所有认证页面、fragment、审计与日志都不得包含密码、服务端/用户密钥、会话令牌或完整 ss:// URI（连接信息页除外）。
func TestNoSecretLeaksAcrossPagesAuditAndLogs(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(logging.New(&logs, slog.LevelDebug))
	t.Cleanup(func() { slog.SetDefault(previous) })

	app := testsupport.New(t)
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	app.SetTraffic(record, 1024, 2048)
	app.Collect()
	userKey, err := app.Keyring.Decrypt(record.Credential.KeyCiphertext, record.Credential.KeyNonce,
		security.SecretAAD("access_credentials", record.Allocation.ID.String(), "user_key", record.Credential.KeyEncryptionVersion))
	if err != nil {
		t.Fatal(err)
	}
	// 服务端密钥按入站生成并加密存储；轮换前取出组合密钥用于泄露扫描。
	info, err := app.Connections.BuildConnectionInfo(context.Background(), record.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 触发一次失败的同步，让日志包含错误路径。
	app.Adapter.Available = false
	if _, err := app.Users.RotateCredential(context.Background(), application.LifecycleInput{ID: record.User.ID, ExpectedRevision: 0, RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	app.Drain()
	app.Adapter.Available = true

	serverURL, _ := url.Parse(app.Server.URL)
	var sessionToken string
	for _, cookie := range app.Client.Jar.Cookies(serverURL) {
		if strings.Contains(cookie.Name, "session") {
			sessionToken = cookie.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("session cookie not found")
	}
	secrets := map[string]string{"password": app.Password, "combined key": info.Password.Reveal(),
		"user key": string(userKey), "session token": sessionToken}
	userPath := "/users/" + record.User.ID.String()
	pages := []string{"/", "/users", "/users?q=ali&status=active", "/users/new", userPath, userPath + "/edit", userPath + "/reset-traffic",
		userPath + "/rotate", userPath + "/delete", "/templates", "/templates/" + templateID.String(), "/templates/" + templateID.String() + "/edit",
		"/settings", "/audit", "/audit?user=" + record.User.ID.String(), "/fragments/dashboard-summary", "/fragments/users-table"}
	for _, path := range pages {
		response, body := app.Get(path)
		if response.StatusCode != 200 {
			t.Fatalf("%s status=%d", path, response.StatusCode)
		}
		for name, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaked the %s", path, name)
			}
		}
		if strings.Contains(body, "ss://") {
			t.Fatalf("%s contains a connection URI", path)
		}
	}
	// 连接信息页是唯一允许展示密钥的页面，但仍不得含会话令牌或密码。
	_, body := app.Get(userPath + "/connection")
	for _, name := range []string{"password", "session token"} {
		if strings.Contains(body, secrets[name]) {
			t.Fatalf("connection page leaked the %s", name)
		}
	}
	rows, err := app.Store.DB().Read.Query(`SELECT COALESCE(safe_summary,'') FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var summary string
		if err := rows.Scan(&summary); err != nil {
			t.Fatal(err)
		}
		for name, secret := range secrets {
			if strings.Contains(summary, secret) || strings.Contains(summary, "ss://") {
				t.Fatalf("audit summary leaked the %s: %q", name, summary)
			}
		}
	}
	output := logs.String()
	for name, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("logs leaked the %s", name)
		}
	}
	if strings.Contains(output, "ss://") {
		t.Fatal("logs contain a connection URI")
	}
}
