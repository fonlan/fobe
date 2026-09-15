package httpapi

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

// --- /install.sh (design §5.2) ---

// installTmplPaths lists candidate locations of scripts/install.sh.tmpl:
// explicit env override first, then repo checkout, then image location.
func (s *Server) installTmplPath() string {
	if s.InstallTmplPath != "" {
		return s.InstallTmplPath
	}
	for _, p := range []string{"scripts/install.sh.tmpl", "/srv/install.sh.tmpl"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// handleInstallScript serves the install script. Token and server URL are
// passed as bash args by default (see the add-node dialog), but the script
// also renders defaults from ?server=&token= so a bare curl works.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	tmplPath := s.installTmplPath()
	if tmplPath == "" {
		writeErr(w, http.StatusNotFound, "install_template_missing")
		return
	}
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	baseURL, err := s.publicBaseURL(r)
	if errors.Is(err, errInvalidPublicURL) {
		writeErr(w, http.StatusBadRequest, "invalid_public_url")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	vars := map[string]string{
		"DefaultServer": shellDoubleQuote(baseURL),
		"DefaultToken":  shellDoubleQuote(r.URL.Query().Get("token")),
	}
	tmpl, err := template.New("install").Parse(string(raw))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// --- /dl: artifact downloads (agent binaries, sing-box releases, design §17) ---

// layout: <DLDir>/agent/<version>/linux-amd64[.sha256]
//
//	<DLDir>/singbox/<version>/linux-amd64
func (s *Server) dlHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.DLDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// normalize and refuse traversal before the file server sees it
		clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/dl"))
		full := filepath.Join(s.DLDir, path.Clean(clean))
		if !strings.HasPrefix(full, filepath.Clean(s.DLDir)+string(os.PathSeparator)) {
			http.NotFound(w, r)
			return
		}
		if s.DLDir == "" {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = clean
		fs.ServeHTTP(w, r)
	})
}

// --- frontend static / API-only mode (design §16) ---

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.WebDir == "" {
		// dev mode: point the user at the Vite dev server instead of 404/blank
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(apiOnlyHTML))
		return
	}

	p := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	full := filepath.Join(s.WebDir, p)
	cleanRoot := filepath.Clean(s.WebDir)
	// allow WebDir itself (request "/") and anything inside it
	if full != cleanRoot && !strings.HasPrefix(full, cleanRoot+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		// SPA fallback: every unknown path serves index.html
		full = filepath.Join(s.WebDir, "index.html")
	}
	http.ServeFile(w, r, full)
}

const apiOnlyHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>fobe</title></head>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;line-height:1.6">
<h1>fobe API-only 模式</h1>
<p>未设置 <code>FOBE_WEB_DIR</code>,server 只提供 API。</p>
<p>请访问 Vite dev server:<a href="http://127.0.0.1:5173"><code>npm run dev</code> → http://127.0.0.1:5173</a></p>
<p style="color:#888">生产部署时前端产物由 Dockerfile 构建进 <code>/srv/web</code>,此页面自动消失。</p>
</body></html>`
