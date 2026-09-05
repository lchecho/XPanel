package handlers

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"

	"xpanel/internal/web/views"
)

type Renderer struct{ Templates *template.Template }

// Page 先渲染到缓冲区，模板错误统一转为 500，避免向浏览器输出半截页面。
func (r Renderer) Page(w http.ResponseWriter, status int, name string, data views.Page) {
	var buffer bytes.Buffer
	if err := r.Templates.ExecuteTemplate(&buffer, name, data); err != nil {
		slog.Default().Error("render template", "template", name, "error_kind", "internal", "detail", err.Error())
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}

// Fragment 渲染不含页面布局的 HTML 片段（HTMX 轮询目标）。
func (r Renderer) Fragment(w http.ResponseWriter, status int, name string, data views.Page) {
	var buffer bytes.Buffer
	if err := r.Templates.ExecuteTemplate(&buffer, name, data); err != nil {
		slog.Default().Error("render fragment", "template", name, "error_kind", "internal", "detail", err.Error())
		http.Error(w, "片段渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}

func (r Renderer) Error(w http.ResponseWriter, status int, message, safeID string) {
	r.Page(w, status, "error.html", views.Page{Title: http.StatusText(status), ErrorSummary: message,
		Values: map[string]string{"error_id": safeID}})
}
