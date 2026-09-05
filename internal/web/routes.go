package web

import (
	"log/slog"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/csrf"

	"xpanel/internal/application"
	"xpanel/internal/web/handlers"
	webmiddleware "xpanel/internal/web/middleware"
	"xpanel/internal/web/views"
)

type RouteDependencies struct {
	Auth        *application.AuthService
	Profiles    *application.ProfileService
	Users       *application.UserService
	Connections *application.ConnectionService
	Settings    *application.SettingsService
	Dashboard   *application.DashboardService
	Sessions    *scs.SessionManager
	CSRFKey     []byte
	Secure      bool
	Ready       func() bool
	Logger      *slog.Logger
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

	base := handlers.Base{Sessions: deps.Sessions, Renderer: renderer, Settings: deps.Settings, Logger: deps.Logger}
	profiles := &handlers.ProfileHandler{Base: base, Service: deps.Profiles}
	users := &handlers.UserHandler{Base: base, Service: deps.Users, Profiles: deps.Profiles, Connections: deps.Connections, Dashboard: deps.Dashboard}

	protected := http.NewServeMux()
	if deps.Dashboard != nil {
		dashboard := &handlers.DashboardHandler{Base: base, Dashboard: deps.Dashboard}
		fragments := &handlers.FragmentHandler{Base: base, Dashboard: deps.Dashboard, Users: deps.Users}
		protected.HandleFunc("GET /{$}", dashboard.Show)
		protected.HandleFunc("GET /fragments/dashboard-summary", fragments.Summary)
		protected.HandleFunc("GET /fragments/users-table", fragments.UsersTable)
	} else {
		protected.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			renderer.Page(w, http.StatusOK, "dashboard.html", views.Page{Title: "仪表盘", Authenticated: true,
				CSRFField: csrf.TemplateField(r), RequestID: handlers.NewRequestID(), Timezone: "UTC",
				Data: views.DashboardView{HealthState: "unknown", HealthLabel: views.HealthLabel("unknown")}})
		})
	}
	protected.HandleFunc("POST /logout", auth.Logout)
	if deps.Profiles != nil {
		protected.HandleFunc("GET /profiles", profiles.List)
		protected.HandleFunc("GET /profiles/new", profiles.NewForm)
		protected.HandleFunc("POST /profiles", profiles.Create)
		protected.HandleFunc("GET /profiles/{profile_id}", profiles.Detail)
		protected.HandleFunc("GET /profiles/{profile_id}/edit", profiles.EditForm)
		protected.HandleFunc("POST /profiles/{profile_id}", profiles.Update)
		protected.HandleFunc("POST /profiles/{profile_id}/revalidate", profiles.Revalidate)
	}
	if deps.Users != nil {
		protected.HandleFunc("GET /users", users.List)
		protected.HandleFunc("GET /users/new", users.NewForm)
		protected.HandleFunc("POST /users", users.Create)
		protected.HandleFunc("GET /users/{user_id}", users.Detail)
		protected.HandleFunc("GET /users/{user_id}/connection", users.Connection)
		protected.HandleFunc("GET /users/{user_id}/edit", users.EditForm)
		protected.HandleFunc("POST /users/{user_id}", users.Update)
		protected.HandleFunc("GET /users/{user_id}/reset-traffic", users.ResetForm)
		protected.HandleFunc("POST /users/{user_id}/reset-traffic", users.Reset)
		protected.HandleFunc("POST /users/{user_id}/enable", users.Enable)
		protected.HandleFunc("POST /users/{user_id}/disable", users.Disable)
		protected.HandleFunc("GET /users/{user_id}/rotate", users.RotateForm)
		protected.HandleFunc("POST /users/{user_id}/rotate", users.Rotate)
		protected.HandleFunc("GET /users/{user_id}/delete", users.DeleteForm)
		protected.HandleFunc("POST /users/{user_id}/delete", users.Delete)
	}
	if deps.Settings != nil {
		settings := &handlers.SettingsHandler{Base: base, Service: deps.Settings}
		protected.HandleFunc("GET /settings", settings.Show)
		protected.HandleFunc("POST /settings", settings.Update)
	}
	public.Handle("/", webmiddleware.RequireAuth(deps.Sessions, deps.Auth.SessionValid, protected))

	stack := webmiddleware.CSRF(deps.CSRFKey, deps.Secure, public)
	stack = deps.Sessions.LoadAndSave(stack)
	return webmiddleware.SecurityHeaders(stack), nil
}
