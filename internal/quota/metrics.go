package quota

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	quotaMetricOnce sync.Once
	quotaDecisions  = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "doctor_quota_decisions_total",
			Help: "Quota admission decisions by scope, resource, and result.",
		},
		[]string{"scope", "resource", "result"},
	)
)

func initQuotaMetrics() {
	quotaMetricOnce.Do(func() {
		prometheus.MustRegister(quotaDecisions)
	})
}

func observeDecision(scope, resource string, accepted bool) {
	initQuotaMetrics()
	result := "accepted"
	if !accepted {
		result = "rejected"
	}
	quotaDecisions.WithLabelValues(scope, resource, result).Inc()
}
