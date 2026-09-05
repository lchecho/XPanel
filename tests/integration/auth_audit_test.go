package integration

import (
	"net/http"
	"net/url"
	"testing"

	"xpanel/internal/domain"
	"xpanel/internal/testsupport"
)

// T140：失败与限流登录写入不枚举、不含密码的 failed 审计；成功登录/登出各有审计。
func TestAuthenticationPathsAreAudited(t *testing.T) {
	app := testsupport.New(t)
	count := func(action string, result domain.AuditResult) int {
		var n int
		if err := app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action=? AND result=?`, action, result).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := 0; i < 5; i++ {
		response, _ := app.PostForm("/login", "/login", url.Values{"username": {"nobody"}, "password": {"wrong-password-attempt"}})
		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt %d status=%d", i, response.StatusCode)
		}
	}
	response, _ := app.PostForm("/login", "/login", url.Values{"username": {"nobody"}, "password": {"wrong-password-attempt"}})
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("throttle status=%d", response.StatusCode)
	}
	if failed := count(domain.ActionLogin, domain.AuditFailed); failed < 6 {
		t.Fatalf("failed login audits = %d", failed)
	}
	var leaked int
	_ = app.Store.DB().Read.QueryRow(`SELECT count(*) FROM audit_events WHERE safe_summary LIKE '%wrong-password%' OR safe_summary LIKE '%nobody%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatalf("audit leaked username or password: %d rows", leaked)
	}
	app.Login()
	if count(domain.ActionLogin, domain.AuditSucceeded) != 1 {
		t.Fatal("successful login not audited exactly once")
	}
	if response, _ = app.PostForm("/logout", "/", nil); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status=%d", response.StatusCode)
	}
	if count(domain.ActionLogout, domain.AuditSucceeded) != 1 {
		t.Fatal("logout not audited")
	}
}
