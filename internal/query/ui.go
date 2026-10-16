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

	// Never let the SPA fallback swallow unimplemented API routes: clients such
	// as Grafana treat a 200 index.html as a valid response and fail decoding it
	// instead of falling back to an older API version. Two guards apply:
	//   - known API prefixes always return 404 (api/, v1/, loki/, ...), and
	//   - any other missing path is only served as the SPA shell when the client
	//     prefers HTML; JSON/API clients get a 404 they can decode.
	if isAPIPath(path) || prefersJSON(r) {
		http.NotFound(w, r)
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

// apiPathPrefixes are the route families doctor-query exposes for API clients.
// A missing route under one of these prefixes must produce a JSON-decodable 404
// (or the handler's own error), never the SPA shell.
var apiPathPrefixes = []string{
	"api/",
	"v1/",
	"loki/",
	"prometheus/",
	"tempo/",
	"zipkin/",
	"grafana/",
}

// isAPIPath reports whether the request path belongs to a known API family.
func isAPIPath(path string) bool {
	for _, prefix := range apiPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// prefersJSON reports whether the caller asked for a machine-readable response
// instead of an HTML navigation. Browsers send Accept: text/html for
// navigations; API clients (Grafana, Perses, curl, SDKs) send application/json,
// a wildcard, or nothing that mentions HTML.
func prefersJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept == "" {
		return false
	}
	return !strings.Contains(accept, "text/html")
}
