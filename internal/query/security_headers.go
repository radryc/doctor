package query

import "net/http"

const doctorCSP = "default-src 'self'; " +
	"base-uri 'self'; " +
	"connect-src 'self'; " +
	"font-src 'self' data:; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"img-src 'self' data:; " +
	"object-src 'none'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'"

// withSecurityHeaders applies a conservative browser baseline to every
// doctor-query response, including the embedded UI and static assets.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", doctorCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
