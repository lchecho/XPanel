package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed templates/layouts/*.html templates/pages/*.html templates/fragments/*.html static/*
var assets embed.FS

// immutableCacheControl 只用于内容哈希命名的静态资源：名称随内容变化，可安全长期缓存（plan: static delivery）。
const immutableCacheControl = "public, max-age=31536000, immutable"

// assetManifest 把逻辑资源名（app.css）映射到内容哈希名（app.<hash>.css），并保存内容用于直接服务。
type assetManifest struct {
	hashed  map[string]string // logical → hashed
	content map[string][]byte // hashed → bytes
}

// buildManifest 为嵌入的 CSS/JS 计算内容哈希名；其余文件（如 THIRD_PARTY.md）不对外提供。
func buildManifest() (*assetManifest, error) {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	manifest := &assetManifest{hashed: map[string]string{}, content: map[string][]byte{}}
	entries, err := fs.ReadDir(static, ".")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		ext := path.Ext(name)
		if entry.IsDir() || (ext != ".css" && ext != ".js") {
			continue
		}
		data, err := fs.ReadFile(static, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		hashedName := strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:])[:12] + ext
		manifest.hashed[name] = hashedName
		manifest.content[hashedName] = data
	}
	return manifest, nil
}

// Path 供模板 `asset` 函数使用：未知资源名在解析期即失败，避免页面静默引用不存在的文件。
func (m *assetManifest) Path(name string) (string, error) {
	hashedName, ok := m.hashed[name]
	if !ok {
		return "", fmt.Errorf("unknown static asset %q", name)
	}
	return "/static/" + hashedName, nil
}

// Handler 只服务哈希名资源并返回 immutable 缓存头；未哈希或未知名称返回 404，认证 HTML 继续由中间件设置 no-store。
func (m *assetManifest) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		data, ok := m.content[name]
		if !ok || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", immutableCacheControl)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}

func parseTemplates(manifest *assetManifest) (*template.Template, error) {
	return template.New("xpanel").Funcs(template.FuncMap{"asset": manifest.Path}).
		ParseFS(assets, "templates/layouts/*.html", "templates/pages/*.html", "templates/fragments/*.html")
}
