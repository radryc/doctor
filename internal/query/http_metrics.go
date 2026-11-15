package query

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	doctorHTTPDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "doctor",
		Subsystem: "query",
		Name:      "http_duration_seconds",
		Help:      "Latency of Doctor query HTTP handlers, by path, method and status.",
		Buckets:   []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"path", "method", "status"})
	doctorHTTPInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "doctor",
		Subsystem: "query",
		Name:      "http_inflight",
		Help:      "In-flight Doctor query HTTP requests by path.",
	}, []string{"path"})
)

func withHTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		doctorHTTPInflight.WithLabelValues(r.URL.Path).Inc()
		defer doctorHTTPInflight.WithLabelValues(r.URL.Path).Dec()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		doctorHTTPDurationSeconds.WithLabelValues(r.URL.Path, r.Method, strconv.Itoa(status)).Observe(time.Since(start).Seconds())
	})
}
