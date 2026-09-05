package query

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed ui/dist
var staticFiles embed.FS

func (s *Service) handleUI(w http.ResponseWriter, r *http.Request) {
	distFS, err := fs.Sub(staticFiles, "ui/dist")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	// Serve the actual file if it exists (JS/CSS/assets)
	if f, err := distFS.Open(path); err == nil {
		_ = f.Close()
		http.FileServer(http.FS(distFS)).ServeHTTP(w, r)
		return
	}

	// SPA fallback: always serve index.html for unknown paths
	data, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
