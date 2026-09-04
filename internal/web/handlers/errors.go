package handlers

import (
	"html/template"
	"net/http"

	"xpanel/internal/web/views"
)

type Renderer struct{ Templates *template.Template }

func (r Renderer) Page(w http.ResponseWriter, status int, name string, data views.Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := r.Templates.ExecuteTemplate(w, name, data); err != nil {
		return
	}
}

func (r Renderer) Error(w http.ResponseWriter, status int, message, safeID string) {
	r.Page(w, status, "error.html", views.Page{Title: http.StatusText(status), ErrorSummary: message,
		Values: map[string]string{"error_id": safeID}})
}
