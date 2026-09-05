package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xpanel/internal/application"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
)

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

// buildBinary 编译一次真实的 xpanel 二进制，供 CLI 契约测试以子进程方式运行。
func buildBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "xpanel-bin")
		if err != nil {
			binaryErr = err
			return
		}
		binaryPath = filepath.Join(dir, "xpanel")
		command := exec.Command("go", "build", "-o", binaryPath, "./cmd/xpanel")
		command.Dir = filepath.Join("..", "..")
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := command.CombinedOutput(); err != nil {
			binaryErr = errors.New("build xpanel: " + string(output))
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

type cliEnvironment struct {
	config   string
	database string
}

func newCLIEnvironment(t *testing.T) cliEnvironment {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "root.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(dataDir, "xpanel.db")
	config := `{
  "server": {"listen": "127.0.0.1:18080", "public_url": "http://127.0.0.1:18080", "insecure_development": true},
  "storage": {"database_path": "` + database + `"},
  "security": {"root_key_file": "` + keyPath + `"},
  "xray": {"api_endpoint": "127.0.0.1:10085"},
  "initial": {"quota_timezone": "UTC"}
}`
	configPath := filepath.Join(root, "xpanel.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return cliEnvironment{config: configPath, database: database}
}

func runCLI(t *testing.T, binary string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func TestAdminInitAndResetPasswordThroughBinary(t *testing.T) {
	binary := buildBinary(t)
	env := newCLIEnvironment(t)
	oldPassword, newPassword := "correct horse battery staple", "another long passphrase 42"

	if code, _, stderr := runCLI(t, binary, "", "admin", "init", "--username", "admin"); code != 2 || stderr == "" {
		t.Fatalf("init without --config code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := runCLI(t, binary, oldPassword+"\n", "admin", "init", "--config", env.config, "--password-stdin"); code != 2 {
		t.Fatalf("init with --password-stdin but without --username code=%d", code)
	}
	code, stdout, stderr := runCLI(t, binary, oldPassword+"\n", "admin", "init", "--config", env.config, "--username", "Admin", "--password-stdin")
	if code != 0 || !strings.Contains(stdout, "administrator initialized") {
		t.Fatalf("init code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, output := range []string{stdout, stderr} {
		if strings.Contains(output, oldPassword) || strings.Contains(output, "$argon2id") {
			t.Fatalf("init output leaked secrets: %q", output)
		}
	}
	if code, _, stderr = runCLI(t, binary, oldPassword+"\n", "admin", "init", "--config", env.config, "--username", "admin", "--password-stdin"); code != 4 || !strings.Contains(stderr, "reset-password") {
		t.Fatalf("second init code=%d stderr=%q", code, stderr)
	}
	if code, _, _ = runCLI(t, binary, newPassword+"\n", "admin", "reset-password", "--config", env.config, "--username", "x", "--password-stdin"); code != 2 {
		t.Fatalf("reset with --username code=%d", code)
	}
	if code, _, _ = runCLI(t, binary, "short\n", "admin", "reset-password", "--config", env.config, "--password-stdin"); code != 4 {
		t.Fatalf("reset with weak password code=%d", code)
	}

	// 登录旧密码成功，并留下一个会话，随后由本机重置撤销。
	db, err := sqlite.Open(context.Background(), env.database, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	auth, err := application.NewAuthService(store, ports.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Login(context.Background(), "admin", []byte(oldPassword), "127.0.0.1")
	if err != nil {
		t.Fatalf("login with the initial password failed: %v", err)
	}
	sessions := sqlite.NewSessionStore(db, 30*time.Minute, 12*time.Hour)
	if err := sessions.Commit("session-token", []byte("data"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := sessions.Find("session-token"); err != nil || !found {
		t.Fatalf("session not found before reset: %v %v", found, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = runCLI(t, binary, newPassword+"\n", "admin", "reset-password", "--config", env.config, "--password-stdin")
	if code != 0 || !strings.Contains(stdout, "administrator password reset") || strings.Contains(stdout+stderr, newPassword) {
		t.Fatalf("reset code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	db, err = sqlite.Open(context.Background(), env.database, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store = sqlite.NewStore(db)
	auth, _ = application.NewAuthService(store, ports.SystemClock{})
	if _, err := auth.Login(context.Background(), "admin", []byte(oldPassword), "127.0.0.1"); !errors.Is(err, application.ErrInvalidCredentials) {
		t.Fatalf("old password still accepted: %v", err)
	}
	if _, err := auth.Login(context.Background(), "admin", []byte(newPassword), "127.0.0.1"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	sessions = sqlite.NewSessionStore(db, 30*time.Minute, 12*time.Hour)
	if _, found, _ := sessions.Find("session-token"); found {
		t.Fatal("existing session survived the password reset")
	}
	var audits int
	var summary string
	if err := db.Read.QueryRow(`SELECT count(*),MAX(safe_summary) FROM audit_events WHERE action=? AND actor_type=?`, domain.ActionPasswordReset, domain.ActorLocalCLI).Scan(&audits, &summary); err != nil || audits != 1 {
		t.Fatalf("password_reset audits = %d, %v", audits, err)
	}
	if strings.Contains(summary, newPassword) || strings.Contains(summary, "$argon2id") {
		t.Fatalf("audit summary leaked secrets: %q", summary)
	}
	var version int64
	_ = db.Read.QueryRow(`SELECT password_version FROM administrators WHERE id=?`, admin.ID.String()).Scan(&version)
	if version != 2 {
		t.Fatalf("password_version = %d", version)
	}
}
