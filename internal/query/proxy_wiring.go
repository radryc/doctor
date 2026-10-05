package query

import (
	"context"
	"fmt"
	"time"

	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// SetProxyRouter installs the multi-backend router. When nil, handlers use the
// legacy single-backend plus AWS path.
func (s *Service) SetProxyRouter(router *proxy.Router) { s.proxyRouter = router }

// ProxyRouter returns the installed router, if any.
func (s *Service) ProxyRouter() *proxy.Router { return s.proxyRouter }

// LocalMetricsUpstream exposes the local PromQL engine and discovery as a
// proxy.MetricsUpstream so it can be registered as the "doctor" backend.
func (s *Service) LocalMetricsUpstream() proxy.MetricsUpstream {
	return localMetricsUpstream{svc: s}
}

// LocalLogsUpstream exposes the local logengine as a proxy.LogsUpstream.
func (s *Service) LocalLogsUpstream() proxy.LogsUpstream {
	return localLogsUpstream{svc: s}
}

type localMetricsUpstream struct {
	svc *Service
}

func (l localMetricsUpstream) Name() string { return proxy.SourceDoctor }

func (l localMetricsUpstream) PromInstant(ctx context.Context, tenant, query string, at time.Time) (map[string]any, error) {
	data, err, parse := l.svc.execPrometheusInstant(ctx, tenant, query, at)
	if err != nil {
		return nil, &proxy.QueryError{Err: err, Parse: parse}
	}
	return data, nil
}

func (l localMetricsUpstream) PromRange(ctx context.Context, tenant, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	data, err, parse := l.svc.execPrometheusRange(ctx, tenant, query, start, end, step)
	if err != nil {
		return nil, &proxy.QueryError{Err: err, Parse: parse}
	}
	return data, nil
}

func (l localMetricsUpstream) PromSeries(ctx context.Context, tenant string, match []string, start, end time.Time) ([]map[string]string, error) {
	selectorSets, err := parsePrometheusSelectorSets(match)
	if err != nil {
		return nil, err
	}
	querier, err := l.svc.localQuerier(tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer func() { _ = querier.Close() }()
	doctorQuerier, ok := querier.(*doctorPromQuerier)
	if !ok {
		return nil, fmt.Errorf("unexpected querier type")
	}
	seen := make(map[string]map[string]string)
	for _, selectorSet := range selectorSets {
		labelMaps, seriesErr := doctorQuerier.seriesLabelMaps(ctx, selectorSet)
		if seriesErr != nil {
			return nil, seriesErr
		}
		for _, labels := range labelMaps {
			seen[metricLabelMapString(labels)] = labels
		}
	}
	out := make([]map[string]string, 0, len(seen))
	for _, labels := range seen {
		out = append(out, labels)
	}
	return out, nil
}

func (l localMetricsUpstream) PromLabels(ctx context.Context, tenant string, match []string, start, end time.Time) ([]string, error) {
	selectorSets, err := parsePrometheusSelectorSets(match)
	if err != nil {
		return nil, err
	}
	querier, err := l.svc.localQuerier(tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer func() { _ = querier.Close() }()

	names := make(map[string]struct{})
	if len(selectorSets) == 0 {
		values, _, labelErr := querier.LabelNames(ctx, nil)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range values {
			names[value] = struct{}{}
		}
		return sortedStrings(names), nil
	}
	for _, selectorSet := range selectorSets {
		values, _, labelErr := querier.LabelNames(ctx, nil, selectorSet...)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range values {
			names[value] = struct{}{}
		}
	}
	return sortedStrings(names), nil
}

func (l localMetricsUpstream) PromLabelValues(ctx context.Context, tenant, label string, match []string, start, end time.Time) ([]string, error) {
	selectorSets, err := parsePrometheusSelectorSets(match)
	if err != nil {
		return nil, err
	}
	querier, err := l.svc.localQuerier(tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer func() { _ = querier.Close() }()

	values := make(map[string]struct{})
	if len(selectorSets) == 0 {
		results, _, labelErr := querier.LabelValues(ctx, label, nil)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range results {
			values[value] = struct{}{}
		}
		return sortedStrings(values), nil
	}
	for _, selectorSet := range selectorSets {
		results, _, labelErr := querier.LabelValues(ctx, label, nil, selectorSet...)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range results {
			values[value] = struct{}{}
		}
	}
	return sortedStrings(values), nil
}

func (s *Service) localQuerier(tenant string, start, end time.Time) (promstorage.Querier, error) {
	start, end = clampDiscoveryWindow(start, end)
	return doctorPromQueryable{backend: s.logEngine, tenant: tenant}.Querier(start.UnixMilli(), end.UnixMilli())
}

type localLogsUpstream struct {
	svc *Service
}

func (l localLogsUpstream) Name() string { return proxy.SourceDoctor }

func (l localLogsUpstream) QueryLogs(ctx context.Context, tenant string, pushdown proxy.LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	query := pushdown.Query()
	if backend, ok := l.svc.logEngine.(rawLogQueryBackend); ok {
		return backend.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	}
	return l.svc.logEngine.QueryLogs(ctx, tenant, query, "", from, to, limit)
}
