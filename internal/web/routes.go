package web

import (
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/csrf"

	"xpanel/internal/application"
	"xpanel/internal/web/handlers"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/web/views"
)

type RouteDependencies struct {
	Auth     *application.AuthService
	Sessions *scs.SessionManager
	CSRFKey  []byte
	Secure   bool
	Ready    func() bool
}

func Routes(deps RouteDependencies) (http.Handler, error) {
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	static, err := staticFiles()
	if err != nil {
		return nil, err
	}
	renderer := handlers.Renderer{Templates: templates}
	auth := &handlers.AuthHandler{Service: deps.Auth, Sessions: deps.Sessions, Renderer: renderer}

	public := http.NewServeMux()
	public.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	public.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	public.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if deps.Ready != nil && deps.Ready() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	public.HandleFunc("GET /login", auth.LoginPage)
	public.HandleFunc("POST /login", auth.Login)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		renderer.Page(w, http.StatusOK, "dashboard.html", views.Page{Title: "仪表盘", Authenticated: true,
			CSRFField: csrf.TemplateField(r), RequestID: handlers.NewRequestID()})
	})
	protected.HandleFunc("POST /logout", auth.Logout)
	public.Handle("/", webmiddleware.RequireAuth(deps.Sessions, deps.Auth.SessionValid, protected))

	stack := webmiddleware.CSRF(deps.CSRFKey, deps.Secure, public)
	stack = deps.Sessions.LoadAndSave(stack)
	return webmiddleware.SecurityHeaders(stack), nil
}
