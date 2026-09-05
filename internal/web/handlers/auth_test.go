package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
)

func TestLoginPageRendersLabelsAndDoesNotEchoPassword(t *testing.T) {
	templates, err := template.New("test").Funcs(template.FuncMap{"asset": func(name string) string { return "/static/" + name }}).
		ParseFiles("../templates/layouts/base.html", "../templates/pages/login.html")
	if err != nil {
		t.Fatal(err)
	}
	sessions := scs.New()
	handler := (&AuthHandler{Sessions: sessions, Renderer: Renderer{Templates: templates}}).LoginPage
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	sessions.LoadAndSave(http.HandlerFunc(handler)).ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `label for="username"`) || !strings.Contains(body, `label for="password"`) {
		t.Fatalf("labels missing: %s", body)
	}
	if strings.Contains(body, "super-secret") {
		t.Fatal("password was echoed")
	}
}
