package query

import (
	"net/http"
	"net/url"
	"strings"
)

// withMutationOriginCheck rejects cross-origin browser mutations and
// cookie-backed mutations without an Origin/Referer header (CSRF defence).
// Bearer-authenticated API clients remain supported.
func withMutationOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		referer := strings.TrimSpace(r.Header.Get("Referer"))
		if origin != "" {
			if !sameOrigin(origin, r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin mutation is not allowed"})
				return
			}
		} else if referer != "" {
			if !sameOrigin(referer, r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin mutation is not allowed"})
				return
			}
		} else if len(r.Cookies()) > 0 && !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "mutation requires an Origin or Referer header"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(raw, requestHost string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, requestHost)
}
