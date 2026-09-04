package web

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed templates/layouts/*.html templates/pages/*.html templates/fragments/*.html static/*
var assets embed.FS

func parseTemplates() (*template.Template, error) {
	return template.New("xpanel").ParseFS(assets, "templates/layouts/*.html", "templates/pages/*.html", "templates/fragments/*.html")
}

func staticFiles() (fs.FS, error) { return fs.Sub(assets, "static") }
