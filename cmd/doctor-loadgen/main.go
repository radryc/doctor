// doctor-loadgen generates artificial Doctor OTLP ingest and query load.
//
//	ingest  OTLP/gRPC logs, metrics and/or traces
//	query   PromQL/Loki/Tempo/SQL HTTP queries
//	http    generic HTTP GET/POST load (agent/guardian endpoints)
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	loadgenDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "loadgen_request_duration_seconds",
		Help:    "Client-side request latency by scenario, op and status.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"scenario", "op", "status"})
	loadgenOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadgen_ops_total",
		Help: "Completed operations by scenario, op and status.",
	}, []string{"scenario", "op", "status"})
	loadgenRecords = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadgen_records_total",
		Help: "Records/items processed by scenario and signal.",
	}, []string{"scenario", "signal"})
	loadgenInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "loadgen_inflight",
		Help: "In-flight requests by scenario.",
	}, []string{"scenario"})
)

type options struct {
	endpoint    string
	token       string
	tenant      string
	signal      string
	service     string
	concurrency int
	duration    time.Duration
	warmup      time.Duration
	rate        float64
	batch       int
	timeout     time.Duration
	jsonOut     string
	metricsAddr string
	label       string

	query       string
	queryFile   string
	queryPath   string
	rangeQuery  bool
	step        string
	rangeWindow time.Duration

	paths  string
	method string
	body   string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	switch os.Args[1] {
	case "ingest":
		err = runIngest(ctx, os.Args[2:])
	case "query":
		err = runQuery(ctx, os.Args[2:])
	case "http":
		err = runHTTP(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `doctor-loadgen <ingest|query|http> [flags]

ingest:
  -endpoint host:port      OTLP gRPC endpoint (default "doctor-ingest.doctor.svc.cluster.local:4317")
  -token string            bearer token (default $DOCTOR_INGEST_AUTH_TOKEN)
  -tenant string           X-Doctor-Tenant (default "perf")
  -signal logs|metrics|traces|all
  -rate float              records/s; 0 = closed loop
  -batch int               records per Export request (default 100)
  -concurrency int         concurrent exporters (default 4)

query:
  -endpoint http://host:port   Doctor query base URL (default "http://doctor-query.doctor.svc.cluster.local:18080")
  -query string                PromQL/Loki/Tempo/SQL query
  -query-file string           file with one query per line
  -query-path string           HTTP path (default /api/v1/query)
  -range                       use range query (adds start/end/step)
  -step duration               range step (default 30s)
  -range-window duration       range window (default 15m)
  -rate float                  queries/s; 0 = closed loop

http:
  -endpoint http://host:port
  -paths /a,/b                 comma-separated paths
  -method GET|POST
  -body string                 request body for POST
  -rate float                  requests/s; 0 = closed loop

common: -concurrency -duration -warmup -timeout -json -metrics-addr -label
`)
}

func parseCommon(name string, args []string) (*options, *flag.FlagSet) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&o.endpoint, "endpoint", envOr("DOCTOR_LOADGEN_ENDPOINT", "doctor-ingest.doctor.svc.cluster.local:4317"), "target endpoint")
	fs.StringVar(&o.token, "token", envOr("DOCTOR_INGEST_AUTH_TOKEN", ""), "bearer token")
	fs.StringVar(&o.tenant, "tenant", "perf", "tenant header")
	fs.StringVar(&o.signal, "signal", "logs", "logs|metrics|traces|all")
	fs.StringVar(&o.service, "service", "perf.loadgen", "service.name resource attribute")
	fs.IntVar(&o.concurrency, "concurrency", 4, "concurrent workers")
	fs.DurationVar(&o.duration, "duration", time.Minute, "measurement duration")
	fs.DurationVar(&o.warmup, "warmup", 10*time.Second, "warm-up duration")
	fs.Float64Var(&o.rate, "rate", 0, "target records/s or queries/s (0 = closed loop)")
	fs.IntVar(&o.batch, "batch", 100, "records per request")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "per-request timeout")
	fs.StringVar(&o.jsonOut, "json", "", "result JSON output path")
	fs.StringVar(&o.metricsAddr, "metrics-addr", ":9100", "Prometheus listen address")
	fs.StringVar(&o.label, "label", name, "scenario label")
	fs.StringVar(&o.query, "query", "up", "query string")
	fs.StringVar(&o.queryFile, "query-file", "", "file with one query per line")
	fs.StringVar(&o.queryPath, "query-path", "/api/v1/query", "query HTTP path")
	fs.BoolVar(&o.rangeQuery, "range", false, "use range query")
	fs.StringVar(&o.step, "step", "30s", "range step")
	fs.DurationVar(&o.rangeWindow, "range-window", 15*time.Minute, "range window")
	fs.StringVar(&o.paths, "paths", "/healthz", "comma-separated HTTP paths")
	fs.StringVar(&o.method, "method", "GET", "HTTP method")
	fs.StringVar(&o.body, "body", "", "HTTP body")
	return o, fs
}

// ---------------------------------------------------------------------------
// ingest

func runIngest(ctx context.Context, args []string) error {
	o, fs := parseCommon("ingest", args)
	fs.Parse(args)
	startMetrics(o.metricsAddr)

	conn, err := grpc.NewClient(o.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	logsClient := collectorlogspb.NewLogsServiceClient(conn)
	metricsClient := collectormetricspb.NewMetricsServiceClient(conn)
	tracesClient := collectortracepb.NewTraceServiceClient(conn)
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: "service.name", Value: strVal(o.service)},
		{Key: "guardian.partition", Value: strVal("perf")},
		{Key: "tenant", Value: strVal(o.tenant)},
	}}

	signals := strings.Split(o.signal, ",")
	if o.signal == "all" {
		signals = []string{"logs", "metrics", "traces"}
	}

	result := newResult("ingest", o)
	measure(ctx, o, "ingest", func(ctx context.Context, w *workerState) error {
		sig := signals[w.rng.IntN(len(signals))]
		callCtx := metadata.AppendToOutgoingContext(ctx, "x-doctor-tenant", o.tenant)
		if o.token != "" {
			callCtx = metadata.AppendToOutgoingContext(callCtx, "authorization", "Bearer "+o.token)
		}
		var records int
		var err error
		start := time.Now()
		switch sig {
		case "logs":
			records = o.batch
			err = sendLogs(callCtx, logsClient, o, res, w, o.batch)
		case "metrics":
			records = o.batch
			err = sendMetrics(callCtx, metricsClient, o, res, w, o.batch)
		case "traces":
			records = o.batch
			err = sendTraces(callCtx, tracesClient, o, res, w, o.batch)
		default:
			return fmt.Errorf("unknown signal %q", sig)
		}
		if ctx.Err() != nil {
			return errStop
		}
		w.observe(sig, start, err, int64(records))
		return err
	}, result)
	return finish(o, result)
}

func sendLogs(ctx context.Context, c collectorlogspb.LogsServiceClient, o *options, res *resourcepb.Resource, w *workerState, n int) error {
	now := uint64(time.Now().UnixNano())
	recs := make([]*logspb.LogRecord, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, &logspb.LogRecord{
			TimeUnixNano:   now,
			SeverityText:   "INFO",
			SeverityNumber: 9,
			Body:           strVal(fmt.Sprintf("perf log %d-%d", w.id, i)),
			Attributes: []*commonpb.KeyValue{
				{Key: "perf.worker", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(w.id)}}},
				{Key: "perf.seq", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(i)}}},
			},
		})
	}
	_, err := c.Export(ctx, &collectorlogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: "perf.loadgen"},
			LogRecords: recs,
		}},
	}}})
	return err
}

func sendMetrics(ctx context.Context, c collectormetricspb.MetricsServiceClient, o *options, res *resourcepb.Resource, w *workerState, n int) error {
	now := uint64(time.Now().UnixNano())
	points := make([]*metricspb.NumberDataPoint, 0, n)
	for i := 0; i < n; i++ {
		points = append(points, &metricspb.NumberDataPoint{
			TimeUnixNano:      now,
			StartTimeUnixNano: now,
			Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: w.rng.Float64()},
			Attributes: []*commonpb.KeyValue{
				{Key: "perf.worker", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(w.id)}}},
			},
		})
	}
	_, err := c.Export(ctx, &collectormetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: res,
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope: &commonpb.InstrumentationScope{Name: "perf.loadgen"},
			Metrics: []*metricspb.Metric{{
				Name:        "perf_loadgen_gauge",
				Description: "synthetic load metric",
				Unit:        "1",
				Data:        &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}},
			}},
		}},
	}}})
	return err
}

func sendTraces(ctx context.Context, c collectortracepb.TraceServiceClient, o *options, res *resourcepb.Resource, w *workerState, n int) error {
	now := uint64(time.Now().UnixNano())
	spans := make([]*tracepb.Span, 0, n)
	for i := 0; i < n; i++ {
		tid := make([]byte, 16)
		sid := make([]byte, 8)
		_, _ = rand.Read(tid)
		_, _ = rand.Read(sid)
		spans = append(spans, &tracepb.Span{
			TraceId:           tid,
			SpanId:            sid,
			Name:              "perf.loadgen.span",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: now,
			EndTimeUnixNano:   now + uint64(time.Millisecond),
			Attributes: []*commonpb.KeyValue{
				{Key: "perf.worker", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(w.id)}}},
			},
		})
	}
	_, err := c.Export(ctx, &collectortracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: res,
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "perf.loadgen"},
			Spans: spans,
		}},
	}}})
	return err
}

// ---------------------------------------------------------------------------
// query

func runQuery(ctx context.Context, args []string) error {
	o, fs := parseCommon("query", args)
	fs.Parse(args)
	o.endpoint = strings.TrimRight(o.endpoint, "/")
	if o.endpoint == "doctor-ingest.doctor.svc.cluster.local:4317" || !strings.HasPrefix(o.endpoint, "http") {
		o.endpoint = "http://doctor-query.doctor.svc.cluster.local:18080"
	}
	startMetrics(o.metricsAddr)
	o.batch = 1
	queries := []string{o.query}
	if o.queryFile != "" {
		b, err := os.ReadFile(o.queryFile)
		if err != nil {
			return err
		}
		queries = nil
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				queries = append(queries, line)
			}
		}
	}
	if len(queries) == 0 {
		return errors.New("no queries configured")
	}
	client := &http.Client{Timeout: o.timeout}
	result := newResult("query", o)
	measure(ctx, o, "query", func(ctx context.Context, w *workerState) error {
		q := queries[w.rng.IntN(len(queries))]
		u := o.endpoint + o.queryPath
		vals := url.Values{}
		vals.Set("query", q)
		if o.rangeQuery {
			end := time.Now()
			start := end.Add(-o.rangeWindow)
			vals.Set("start", fmt.Sprintf("%d", start.Unix()))
			vals.Set("end", fmt.Sprintf("%d", end.Unix()))
			vals.Set("step", o.step)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+vals.Encode(), nil)
		req.Header.Set("X-Doctor-Tenant", o.tenant)
		if o.token != "" {
			req.Header.Set("Authorization", "Bearer "+o.token)
		}
		start := time.Now()
		resp, err := client.Do(req)
		var n int64
		if err == nil {
			n, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				err = fmt.Errorf("http %d", resp.StatusCode)
			}
		}
		if ctx.Err() != nil {
			return errStop
		}
		w.observe("query", start, err, n)
		return err
	}, result)
	return finish(o, result)
}

// ---------------------------------------------------------------------------
// generic HTTP

func runHTTP(ctx context.Context, args []string) error {
	o, fs := parseCommon("http", args)
	fs.Parse(args)
	o.endpoint = strings.TrimRight(o.endpoint, "/")
	if !strings.HasPrefix(o.endpoint, "http") {
		o.endpoint = "http://" + o.endpoint
	}
	startMetrics(o.metricsAddr)
	o.batch = 1
	paths := []string{o.paths}
	if strings.Contains(o.paths, ",") {
		paths = strings.Split(o.paths, ",")
	}
	client := &http.Client{Timeout: o.timeout}
	result := newResult("http", o)
	measure(ctx, o, "http", func(ctx context.Context, w *workerState) error {
		p := strings.TrimSpace(paths[w.rng.IntN(len(paths))])
		var body io.Reader
		if o.body != "" {
			body = bytes.NewBufferString(o.body)
		}
		req, _ := http.NewRequestWithContext(ctx, o.method, o.endpoint+p, body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Doctor-Tenant", o.tenant)
		if o.token != "" {
			req.Header.Set("Authorization", "Bearer "+o.token)
		}
		start := time.Now()
		resp, err := client.Do(req)
		var n int64
		if err == nil {
			n, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				err = fmt.Errorf("http %d", resp.StatusCode)
			}
		}
		if ctx.Err() != nil {
			return errStop
		}
		w.observe(p, start, err, n)
		return err
	}, result)
	return finish(o, result)
}

// ---------------------------------------------------------------------------
// workload + result plumbing

var (
	errStop          = errors.New("loadgen: stop")
	measurementStart atomic.Int64
	currentScenario  atomic.Value
	errorCodes       sync.Map
)

type workerState struct {
	id            int
	rng           *mrand.Rand
	samples       []float64
	measured      int64
	measuredErrs  int64
	measuredBytes int64
	mu            sync.Mutex
}

func (w *workerState) observe(op string, start time.Time, err error, nbytes int64) {
	d := time.Since(start).Seconds()
	code := "ok"
	if err != nil {
		code = status.Code(err).String()
		if code == "OK" {
			code = "error"
		}
		if strings.HasPrefix(err.Error(), "http ") {
			code = strings.ReplaceAll(err.Error(), " ", "_")
		}
	}
	scenario := scenarioLabel()
	loadgenDuration.WithLabelValues(scenario, op, code).Observe(d)
	loadgenOps.WithLabelValues(scenario, op, code).Inc()
	if nbytes > 0 {
		loadgenRecords.WithLabelValues(scenario, op).Add(float64(nbytes))
	}
	if err != nil {
		key := scenario + "|" + code
		c, _ := errorCodes.LoadOrStore(key, &atomic.Int64{})
		c.(*atomic.Int64).Add(1)
	}
	w.mu.Lock()
	measuring := measurementStart.Load()
	if measuring > 0 && time.Now().UnixNano() >= measuring {
		w.measured++
		w.measuredBytes += nbytes
		if err != nil {
			w.measuredErrs++
		}
		if len(w.samples) < 200000 {
			w.samples = append(w.samples, d)
		}
	}
	w.mu.Unlock()
}

func scenarioLabel() string {
	if v := currentScenario.Load(); v != nil {
		return v.(string)
	}
	return "unknown"
}

type workload func(context.Context, *workerState) error

func measure(ctx context.Context, o *options, scenario string, op workload, r *result) {
	currentScenario.Store(scenario)
	runCtx, cancel := context.WithTimeout(ctx, o.warmup+o.duration)
	defer cancel()
	start := time.Now()
	measureStart := start.Add(o.warmup)
	measurementStart.Store(measureStart.UnixNano())

	workers := make([]*workerState, o.concurrency)
	for i := range workers {
		workers[i] = &workerState{id: i, rng: mrand.New(mrand.NewPCG(uint64(time.Now().UnixNano()), uint64(i+1)))}
	}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *workerState) {
			defer wg.Done()
			interval := time.Duration(0)
			if o.rate > 0 {
				batch := o.batch
				if batch <= 0 {
					batch = 1
				}
				per := o.rate / float64(batch) / float64(o.concurrency)
				if per < 1 {
					per = 1
				}
				interval = time.Duration(float64(time.Second) / per)
			}
			var next time.Time
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				now := time.Now()
				loadgenInflight.WithLabelValues(scenario).Inc()
				_ = op(runCtx, w)
				loadgenInflight.WithLabelValues(scenario).Dec()
				if interval > 0 {
					next = next.Add(interval)
					if next.IsZero() || next.Before(now) {
						next = now.Add(interval)
					}
					select {
					case <-runCtx.Done():
						return
					case <-time.After(time.Until(next)):
					}
				}
			}
		}(w)
	}
	wg.Wait()

	r.DurationSeconds = time.Since(measureStart).Seconds()
	if r.DurationSeconds <= 0 {
		r.DurationSeconds = time.Since(start).Seconds()
	}
	var samples []float64
	for _, w := range workers {
		w.mu.Lock()
		r.Ops += w.measured
		r.Errors += w.measuredErrs
		r.Bytes += w.measuredBytes
		samples = append(samples, w.samples...)
		w.mu.Unlock()
	}
	sort.Float64s(samples)
	r.Samples = len(samples)
	r.P50MS = pct(samples, 0.50)
	r.P95MS = pct(samples, 0.95)
	r.P99MS = pct(samples, 0.99)
	r.MaxMS = pct(samples, 1.0)
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i] * 1000
}

type result struct {
	Scenario        string           `json:"scenario"`
	StartedAt       string           `json:"started_at"`
	DurationSeconds float64          `json:"duration_seconds"`
	Ops             int64            `json:"ops"`
	OpsPerSec       float64          `json:"ops_per_sec"`
	Bytes           int64            `json:"bytes"`
	Errors          int64            `json:"errors"`
	ErrorRate       float64          `json:"error_rate"`
	ErrorBreakdown  map[string]int64 `json:"error_breakdown,omitempty"`
	Samples         int              `json:"samples"`
	P50MS           float64          `json:"p50_ms"`
	P95MS           float64          `json:"p95_ms"`
	P99MS           float64          `json:"p99_ms"`
	MaxMS           float64          `json:"max_ms"`
}

func newResult(scenario string, o *options) *result {
	return &result{Scenario: scenario, StartedAt: time.Now().UTC().Format(time.RFC3339)}
}

func finish(o *options, r *result) error {
	if r.DurationSeconds > 0 {
		r.OpsPerSec = float64(r.Ops) / r.DurationSeconds
	}
	if r.Ops > 0 {
		r.ErrorRate = float64(r.Errors) / float64(r.Ops)
	}
	r.ErrorBreakdown = map[string]int64{}
	errorCodes.Range(func(k, v any) bool {
		key := k.(string)
		prefix := r.Scenario + "|"
		if strings.HasPrefix(key, prefix) {
			r.ErrorBreakdown[strings.TrimPrefix(key, prefix)] = v.(*atomic.Int64).Load()
		}
		return true
	})
	if o.jsonOut != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(o.jsonOut, b, 0644); err != nil {
			return err
		}
	}
	fmt.Printf("\n%s: %d ops, %.2f ops/s, p50=%.2fms p95=%.2fms p99=%.2fms, errors=%d (%.3f%%)\n",
		r.Scenario, r.Ops, r.OpsPerSec, r.P50MS, r.P95MS, r.P99MS, r.Errors, r.ErrorRate*100)
	return nil
}

func startMetrics(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	go func() { _ = http.ListenAndServe(addr, mux) }()
}

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
