package handlers_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"xpanel/internal/application"
	"xpanel/internal/testsupport"
)

var (
	inputPattern    = regexp.MustCompile(`<(input|select|textarea)\b[^>]*>`)
	idPattern       = regexp.MustCompile(`\bid="([^"]+)"`)
	typePattern     = regexp.MustCompile(`\btype="([^"]+)"`)
	describedBy     = regexp.MustCompile(`aria-describedby="([^"]+)"`)
	inlineScript    = regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]*\S[^<]*</script>`)
	inlineHandler   = regexp.MustCompile(`\son[a-z]+="`)
	tablePattern    = regexp.MustCompile(`(?s)<table>.*?</table>`)
	requiredMediaQ  = "@media (max-width: 40rem)"
	stylesheetPath  = "../static/app.css"
	navigationLinks = []string{`<a href="/">`, `<a href="/users">`, `<a href="/templates">`, `<a href="/audit">`, `<a href="/settings">`}
)

// 所有页面：每个可见输入有程序化标签，错误提示通过 aria-describedby 关联，表格有 caption 与 scope，
// 无内联脚本与内联事件处理器，导航为链接、动作为按钮（http.md §Accessibility and Progressive Enhancement）。
func TestPagesMeetStructuralAccessibilityRules(t *testing.T) {
	app := testsupport.New(t)
	_, login := app.Get("/login")
	app.Login()
	templateID := app.RegisterCompatibleTemplate("Primary")
	record := app.CreateUser("Alice", templateID, nil)
	app.Drain()
	app.SetTraffic(record, 512, 512)
	app.Collect()
	if _, err := app.Users.SetAdminEnabled(context.Background(), application.SetEnabledInput{ID: record.User.ID, Enabled: false, ExpectedRevision: 0,
		RequestID: testsupport.NewID(t), ActorID: app.AdminID}); err != nil {
		t.Fatal(err)
	}
	userPath := "/users/" + record.User.ID.String()
	pages := map[string]string{"/login": login}
	for _, path := range []string{"/", "/users", "/users/new", userPath, userPath + "/edit", userPath + "/reset-traffic", userPath + "/rotate",
		userPath + "/delete", userPath + "/connection", "/templates", "/templates/new", "/templates/" + templateID.String(), "/templates/" + templateID.String() + "/edit",
		"/settings", "/audit"} {
		response, body := app.Get(path)
		if response.StatusCode != 200 {
			t.Fatalf("%s status=%d", path, response.StatusCode)
		}
		pages[path] = body
	}
	// 一个 422 页面：错误摘要可聚焦。
	_, errorPage := app.PostForm("/settings", "/settings", map[string][]string{"quota_timezone": {"Nowhere/City"}, "_version": {"0"}})
	pages["/settings (422)"] = errorPage

	for path, body := range pages {
		if inlineScript.MatchString(body) || inlineHandler.MatchString(body) {
			t.Fatalf("%s contains inline script or inline event handlers", path)
		}
		for _, match := range inputPattern.FindAllString(body, -1) {
			if typeMatch := typePattern.FindStringSubmatch(match); len(typeMatch) == 2 && (typeMatch[1] == "hidden" || typeMatch[1] == "checkbox") {
				continue
			}
			id := idPattern.FindStringSubmatch(match)
			if len(id) != 2 {
				t.Fatalf("%s has a control without id: %s", path, match)
			}
			if !strings.Contains(body, `<label for="`+id[1]+`"`) {
				t.Fatalf("%s control %q lacks a programmatic label", path, id[1])
			}
			for _, described := range describedBy.FindAllStringSubmatch(match, -1) {
				for _, ref := range strings.Fields(described[1]) {
					if !strings.Contains(body, `id="`+ref+`"`) {
						t.Fatalf("%s aria-describedby references missing id %q", path, ref)
					}
				}
			}
		}
		for _, table := range tablePattern.FindAllString(body, -1) {
			if !strings.Contains(table, "<caption>") || !strings.Contains(table, `scope="col"`) {
				t.Fatalf("%s has a table without caption or column scope", path)
			}
		}
		if path != "/login" {
			for _, link := range navigationLinks {
				if !strings.Contains(body, link) {
					t.Fatalf("%s lacks navigation link %s", path, link)
				}
			}
			if !strings.Contains(body, `aria-live="polite"`) {
				t.Fatalf("%s lacks a polite live region", path)
			}
		}
		if strings.Contains(path, "422") && !strings.Contains(body, `class="error-summary" tabindex="-1" role="alert"`) {
			t.Fatalf("%s lacks a focusable error summary", path)
		}
	}
	css, err := os.ReadFile(stylesheetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), requiredMediaQ) || !strings.Contains(string(css), ":focus-visible") {
		t.Fatal("stylesheet lacks the mobile breakpoint or visible focus styles")
	}
}
