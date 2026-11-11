package query

import (
	"context"
	"time"

	"github.com/radryc/doctor/internal/proxy"
)

// gcpDiscoveryReady reports whether a GCP backend is configured. There is no
// cheap health probe in the GCP client, so configuration is enough here;
// failures surface as request warnings instead of hiding the local catalog.
func (s *Service) gcpDiscoveryReady() bool {
	return s != nil && s.gcp != nil && s.gcp.Enabled()
}

// gcpMetricNames returns every Cloud Monitoring metric name. The pusher
// returns the full descriptor catalog (tens of thousands of entries for a
// project), so the result is cached briefly and filtered/limited by the caller.
func (s *Service) gcpMetricNames(ctx context.Context) []string {
	if !s.gcpDiscoveryReady() {
		return nil
	}
	load := func(ctx context.Context) (any, error) {
		callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		resp, err := s.gcp.PrometheusLabelValues(callCtx, "__name__", nil, time.Time{}, time.Time{})
		if err != nil {
			return nil, err
		}
		return extractPrometheusStringList(resp), nil
	}
	// getMetricsCache handles a nil receiver by executing the loader directly,
	// so this works with result caching disabled as well.
	value, err := s.getMetricsCache().getOrLoad(ctx, "gcp-metric-names", load)
	if err != nil {
		proxy.AddWarning(ctx, proxy.Warning{
			Backend: proxy.SourceGCP,
			Signal:  proxy.SignalMetrics,
			Message: err.Error(),
		})
		return nil
	}
	names, _ := value.([]string)
	// Callers sort and filter discovery results; never expose the cached slice.
	return append([]string(nil), names...)
}
