package http

import (
	"embed"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dashboard_assets
var dashboardFS embed.FS

// NewStaticHandler serves the embedded Next.js static export with SPA fallback.
// API routes under /v1 and /api are left to other handlers; the ServeMux
// longest-prefix match ensures those routes take precedence when registered
// before this catch-all.
func NewStaticHandler(log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return &staticHandler{log: log}
}

type staticHandler struct {
	log *slog.Logger
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	p := r.URL.Path
	clean := path.Clean(p)
	if clean == "." {
		clean = "/"
	}

	// Never serve static assets for API routes.
	if strings.HasPrefix(clean, "/v1/") || strings.HasPrefix(clean, "/api/") {
		http.NotFound(w, r)
		return
	}

	// Resolve the static-export file for the cleaned path. Next.js static
	// export lays out each route as <route>.html (prerendered page) plus a
	// <route>/ directory holding nested routes and prerender data — but NOT a
	// <route>/index.html for every route. So resolution order is:
	//   1. exact file (asset paths like _next/static/...)
	//   2. <route>/index.html (when the export produced one)
	//   3. <route>.html (prerendered page for the route itself)
	//   4. SPA fallback index.html (client-side routing)
	// A bare directory candidate (e.g. "/dashboard") must never be served as
	// a file — it resolves via step 2/3 instead, otherwise the browser
	// downloads an octet-stream directory entry.
	candidate := strings.TrimPrefix(clean, "/")
	if candidate == "" {
		candidate = "index.html"
	}

	// 1. Exact file — but skip it if it is a directory (fall through to 2/3).
	if file, err := dashboardFS.Open(path.Join("dashboard_assets", candidate)); err == nil {
		if info, statErr := file.Stat(); statErr == nil && !info.IsDir() {
			defer file.Close()
			serveFile(w, r, candidate, file)
			return
		}
		file.Close()
	}

	// 2. Directory index: <route>/index.html.
	indexPath := path.Join("dashboard_assets", candidate, "index.html")
	if file, err := dashboardFS.Open(indexPath); err == nil {
		defer file.Close()
		serveFile(w, r, path.Join(candidate, "index.html"), file)
		return
	}

	// 3. Prerendered page: <route>.html. Skip the root (handled by step 1).
	if candidate != "index.html" {
		pagePath := path.Join("dashboard_assets", candidate+".html")
		if file, err := dashboardFS.Open(pagePath); err == nil {
			defer file.Close()
			serveFile(w, r, candidate+".html", file)
			return
		}
	}

	// 4. SPA fallback: any non-API path gets index.html.
	fallback, err := dashboardFS.Open("dashboard_assets/index.html")
	if err != nil {
		h.log.Error("missing embedded dashboard index", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer fallback.Close()
	serveFile(w, r, "index.html", fallback)
}

func serveFile(w http.ResponseWriter, r *http.Request, relPath string, file fs.File) {
	info, err := file.Stat()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	ct := mime.TypeByExtension(path.Ext(relPath))
	if ct == "" {
		if strings.HasSuffix(relPath, ".html") {
			ct = "text/html; charset=utf-8"
		} else {
			ct = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", ct)

	// Apply long cache headers for fingerprinted Next.js assets.
	if isFingerprintedAsset(relPath) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}
	_ = copyBytes(w, file, info.Size())
}

func isFingerprintedAsset(relPath string) bool {
	base := path.Base(relPath)
	return strings.Contains(base, "-") && strings.ContainsAny(path.Ext(base), "0123456789abcdef")
}

func copyBytes(w io.Writer, src io.Reader, size int64) error {
	if size > 0 {
		_, err := io.CopyN(w, src, size)
		return err
	}
	_, err := io.Copy(w, src)
	return err
}
