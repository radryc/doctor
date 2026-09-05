package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/buildinfo"
	"github.com/rydzu/ainfra/doctor/internal/otlp"
	"github.com/rydzu/ainfra/doctor/internal/privacy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Flusher is the backend used by Service, buffers, and pipelines to persist telemetry records.
type Flusher interface {
	WriteTraces(ctx context.Context, records []segment.TraceRecord) error
	WriteLogs(ctx context.Context, records []segment.LogRecord) error
	WriteMetrics(ctx context.Context, records []segment.MetricPointRecord) error
	WriteGuardianEvents(ctx context.Context, events []segment.GuardianEvent) error
}

type Service struct {
	tenantHeader  string
	defaultTenant string
	writer        Flusher
	pipeline      *Pipeline
	traceBuffer   *TraceBuffer
	logBuffer     *LogBuffer
	metricBuffer  *MetricBuffer
	privacy       *privacy.Engine
}

func NewService(tenantHeader, defaultTenant string, writer Flusher, privacyEngine *privacy.Engine) *Service {
	return NewServiceWithBuffers(tenantHeader, defaultTenant, writer, nil, nil, nil, nil, privacyEngine)
}

func NewServiceWithTraceBuffer(tenantHeader, defaultTenant string, writer Flusher, traceBuffer *TraceBuffer, privacyEngine *privacy.Engine) *Service {
	return NewServiceWithBuffers(tenantHeader, defaultTenant, writer, nil, traceBuffer, nil, nil, privacyEngine)
}

func NewServiceWithPipeline(tenantHeader, defaultTenant string, writer Flusher, pipeline *Pipeline, privacyEngine *privacy.Engine) *Service {
	return NewServiceWithBuffers(tenantHeader, defaultTenant, writer, pipeline, nil, nil, nil, privacyEngine)
}

func NewServiceWithBuffers(tenantHeader, defaultTenant string, writer Flusher, pipeline *Pipeline, traceBuffer *TraceBuffer, logBuffer *LogBuffer, metricBuffer *MetricBuffer, privacyEngine *privacy.Engine) *Service {
	primeAcceptedIngestMetrics()
	return &Service{
		tenantHeader:  tenantHeader,
		defaultTenant: defaultTenant,
		writer:        writer,
		pipeline:      pipeline,
		traceBuffer:   traceBuffer,
		logBuffer:     logBuffer,
		metricBuffer:  metricBuffer,
		privacy:       privacyEngine,
	}
}

func (s *Service) RegisterGRPC(server *grpc.Server) {
	coltracepb.RegisterTraceServiceServer(server, &traceServer{svc: s})
	collogspb.RegisterLogsServiceServer(server, &logsServer{svc: s})
	colmetricspb.RegisterMetricsServiceServer(server, &metricsServer{svc: s})
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/v1/status/buildinfo", s.handleBuildInfo)
	mux.HandleFunc("/v1/traces", s.handleHTTPTraces)
	mux.HandleFunc("/v1/logs", s.handleHTTPLogs)
	mux.HandleFunc("/v1/metrics", s.handleHTTPMetrics)
	mux.HandleFunc("/v1/guardian/events", s.handleGuardianEvents)
	return mux
}

func (s *Service) ingestTraces(ctx context.Context, tenant string, req *coltracepb.ExportTraceServiceRequest) error {
	records := otlp.FlattenTraces(req, tenant, s.privacy)
	if s.pipeline != nil {
		if err := s.pipeline.SubmitTraces(ctx, records); err != nil {
			return err
		}
		recordAcceptedIngest(ctx, "traces", len(records))
		return nil
	}
	if s.traceBuffer != nil {
		if err := s.traceBuffer.Append(ctx, records); err != nil {
			return err
		}
		recordAcceptedIngest(ctx, "traces", len(records))
		return nil
	}
	if err := s.writer.WriteTraces(ctx, records); err != nil {
		return err
	}
	recordAcceptedIngest(ctx, "traces", len(records))
	return nil
}

func (s *Service) ingestLogs(ctx context.Context, tenant string, req *collogspb.ExportLogsServiceRequest) error {
	records := otlp.FlattenLogs(req, tenant, s.privacy)
	if s.pipeline != nil {
		if err := s.pipeline.SubmitLogs(ctx, records); err != nil {
			return err
		}
		recordAcceptedIngest(ctx, "logs", len(records))
		return nil
	}
	if s.logBuffer != nil {
		if err := s.logBuffer.Append(ctx, records); err != nil {
			return err
		}
		recordAcceptedIngest(ctx, "logs", len(records))
		return nil
	}
	if err := s.writer.WriteLogs(ctx, records); err != nil {
		return err
	}
	recordAcceptedIngest(ctx, "logs", len(records))
	return nil
}

func (s *Service) ingestMetrics(ctx context.Context, tenant string, req *colmetricspb.ExportMetricsServiceRequest) error {
	records := otlp.FlattenMetrics(req, tenant, s.privacy)
	if s.pipeline != nil {
		if err := s.pipeline.SubmitMetrics(ctx, records); err != nil {
			return err
		}
		recordAcceptedIngest(ctx, "metrics", len(records))
		return nil
	}
	if s.metricBuffer != nil {
		if err := s.metricBuffer.Append(ctx, records); err != nil {
			return err
		}
		recordAcceptedIngest(ctx, "metrics", len(records))
		return nil
	}
	if err := s.writer.WriteMetrics(ctx, records); err != nil {
		return err
	}
	recordAcceptedIngest(ctx, "metrics", len(records))
	return nil
}

func (s *Service) tenantFromHeader(r *http.Request) string {
	tenant := ""
	if r != nil {
		tenant = strings.TrimSpace(r.Header.Get(s.tenantHeader))
	}
	return s.fallbackTenant(tenant)
}

func (s *Service) tenantFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return s.fallbackTenant("")
	}
	values := md.Get(strings.ToLower(s.tenantHeader))
	if len(values) == 0 {
		values = md.Get(s.tenantHeader)
	}
	if len(values) == 0 {
		return s.fallbackTenant("")
	}
	return s.fallbackTenant(values[0])
}

func (s *Service) fallbackTenant(tenant string) string {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		tenant = s.defaultTenant
	}
	if tenant == "" {
		return "default"
	}
	return tenant
}

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data":   buildinfo.Current().StatusFields(),
	})
}

func (s *Service) handleHTTPTraces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.ingestTraces(r.Context(), s.tenantFromHeader(r), &req); err != nil {
		http.Error(w, err.Error(), httpIngestStatus(err))
		return
	}
	resp := &coltracepb.ExportTraceServiceResponse{}
	writeProto(w, http.StatusAccepted, resp)
}

func (s *Service) handleHTTPLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.ingestLogs(r.Context(), s.tenantFromHeader(r), &req); err != nil {
		http.Error(w, err.Error(), httpIngestStatus(err))
		return
	}
	resp := &collogspb.ExportLogsServiceResponse{}
	writeProto(w, http.StatusAccepted, resp)
}

func (s *Service) handleHTTPMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.ingestMetrics(r.Context(), s.tenantFromHeader(r), &req); err != nil {
		http.Error(w, err.Error(), httpIngestStatus(err))
		return
	}
	resp := &colmetricspb.ExportMetricsServiceResponse{}
	writeProto(w, http.StatusAccepted, resp)
}

func (s *Service) handleGuardianEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MB limit
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var events []segment.GuardianEvent
	if err := json.Unmarshal(raw, &events); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(events) == 0 {
		http.Error(w, "empty event list", http.StatusBadRequest)
		return
	}
	tenant := s.tenantFromHeader(r)
	now := time.Now().UTC()
	for i := range events {
		if events[i].Tenant == "" {
			events[i].Tenant = tenant
		}
		if events[i].Timestamp.IsZero() {
			events[i].Timestamp = now
		}
	}
	if err := s.writer.WriteGuardianEvents(r.Context(), events); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": len(events),
	})
}

func writeProto(w http.ResponseWriter, statusCode int, msg proto.Message) {
	payload, err := proto.Marshal(msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(statusCode)
	_, _ = w.Write(payload)
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(value)
}

type traceServer struct {
	coltracepb.UnimplementedTraceServiceServer
	svc *Service
}

func (s *traceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if err := s.svc.ingestTraces(ctx, s.svc.tenantFromContext(ctx), req); err != nil {
		return nil, grpcIngestError(err)
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type logsServer struct {
	collogspb.UnimplementedLogsServiceServer
	svc *Service
}

func (s *logsServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if err := s.svc.ingestLogs(ctx, s.svc.tenantFromContext(ctx), req); err != nil {
		return nil, grpcIngestError(err)
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

type metricsServer struct {
	colmetricspb.UnimplementedMetricsServiceServer
	svc *Service
}

func (s *metricsServer) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if err := s.svc.ingestMetrics(ctx, s.svc.tenantFromContext(ctx), req); err != nil {
		return nil, grpcIngestError(err)
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// grpcIngestError maps ingest errors to appropriate gRPC status codes so OTLP
// clients apply the correct retry/backoff policy.
func grpcIngestError(err error) error {
	if errors.Is(err, ErrBufferFull) {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return err
}

// httpIngestStatus maps ingest errors to the appropriate HTTP status code.
func httpIngestStatus(err error) int {
	if errors.Is(err, ErrBufferFull) {
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}
