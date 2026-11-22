package query

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"

	doctorDisplay "github.com/radryc/doctor/internal/display"
	coreauthz "github.com/radryc/monofs/pkg/authz"
	shared "github.com/radryc/monofs/pkg/display"
)

func (s *Service) handleDisplayEffective(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	profile, version, err := s.displayProfiles.Resolve(r.Context(), displaySubjects(r), "")
	if err != nil || profile == nil {
		writeJSON(w, http.StatusOK, map[string]string{"profile": ""})
		return
	}
	if version != "" {
		w.Header().Set(shared.VersionHeader, version)
	}
	writeJSON(w, http.StatusOK, map[string]string{"profile": profile.ID})
}

func (s *Service) handleDisplayProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	profiles, version, err := s.displayProfiles.Profiles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "display bundle unavailable"})
		return
	}
	if version != "" {
		w.Header().Set(shared.VersionHeader, version)
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles, "version": version})
}

func (s *Service) withDisplayMapping(next http.Handler) http.Handler {
	if s == nil || s.displayProfiles == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if translated := s.displayProfiles.TranslateQuery(r.Context(), r.URL.Query()); len(translated) > 0 {
			r.URL.RawQuery = url.Values(translated).Encode()
		}
		requested := strings.TrimSpace(r.Header.Get(shared.HeaderName))
		if requested == "" {
			requested = strings.TrimSpace(r.URL.Query().Get(shared.QueryParam))
		}
		if !displaySurface(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		subjects := displaySubjects(r)
		profile, version, err := s.displayProfiles.Resolve(r.Context(), subjects, requested)
		if err != nil || profile == nil {
			next.ServeHTTP(w, r)
			return
		}
		buffer := &displayBuffer{header: make(http.Header)}
		next.ServeHTTP(buffer, r)
		body := buffer.body.Bytes()
		if buffer.status >= 200 && buffer.status < 300 && strings.Contains(buffer.header.Get("Content-Type"), "json") {
			keepCanonical := strings.EqualFold(strings.TrimSpace(r.Header.Get(shared.KeepCanonicalHeader)), "1") ||
				strings.EqualFold(strings.TrimSpace(r.Header.Get(shared.KeepCanonicalHeader)), "true")
			if mapped, mapErr := shared.MapJSONWithOptions(body, profile, shared.MapOptions{
				Surface:       displaySurfaceName(r.URL.Path),
				KeepCanonical: keepCanonical,
			}); mapErr == nil {
				body = mapped
			}
		}
		for key, values := range buffer.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Del("Content-Length")
		w.Header().Set(shared.HeaderName, profile.ID)
		if version != "" {
			w.Header().Set(shared.VersionHeader, version)
		}
		w.Header().Add("Vary", shared.HeaderName)
		w.Header().Set("Cache-Control", "private")
		status := buffer.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

type displayBuffer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *displayBuffer) Header() http.Header { return b.header }
func (b *displayBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *displayBuffer) Write(data []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(data)
}

func displaySurface(path string) bool {
	if strings.HasPrefix(path, "/v1/audit") || strings.HasPrefix(path, "/v1/display") {
		return false
	}
	for _, prefix := range []string{"/v1/", "/loki/", "/api/", "/api/v2/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func displaySurfaceName(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/v1/"):
		return "metrics"
	case strings.HasPrefix(path, "/loki/"):
		return "logs"
	case strings.HasPrefix(path, "/api/v2/"):
		return "traces"
	case strings.HasPrefix(path, "/v1/sql"):
		return "sql"
	}
	return ""
}

func displaySubjects(r *http.Request) []string {
	id, _ := coreauthz.IdentityFromContext(r.Context())
	who := strings.TrimSpace(id.Email)
	if who == "" {
		who = strings.TrimSpace(id.Subject)
	}
	if who == "" {
		who = strings.TrimSpace(id.ClientID)
	}
	if who == "" {
		return nil
	}
	out := []string{"user:" + who}
	for _, group := range id.Groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		out = append(out, group, "team:"+group)
	}
	return out
}

// SetDisplayProfiles installs the display bundle loader. Nil disables mapping.
func (s *Service) SetDisplayProfiles(service *doctorDisplay.Service) {
	if s != nil {
		s.displayProfiles = service
	}
}
