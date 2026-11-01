package query

import "net/http"

const maxDoctorRequestBodyBytes = 32 << 20

func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, maxDoctorRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
