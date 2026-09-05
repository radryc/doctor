package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/config"
	"github.com/rydzu/ainfra/doctor/internal/ingest"
	"github.com/rydzu/ainfra/doctor/internal/privacy"
	"github.com/rydzu/ainfra/doctor/internal/telemetry"
	"github.com/rydzu/ainfra/doctor/internal/wal"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Set a soft memory limit at 80% of the container limit so GC runs
	// aggressively before the kernel OOM killer fires.
	const memLimit = 858 << 20 // 858 MiB (~80% of 1 GiB)
	debug.SetMemoryLimit(memLimit)
	log.Printf("GOMEMLIMIT set to %dMiB", memLimit>>20)

	cfg, err := config.LoadIngest("doctor-ingest")
	if err != nil {
		log.Fatal(err)
	}

	if cfg.DebugAddr != "" {
		go func() {
			log.Printf("pprof debug server listening on %s", cfg.DebugAddr)
			if err := http.ListenAndServe(cfg.DebugAddr, nil); err != nil {
				log.Printf("pprof debug server: %v", err)
			}
		}()
	}
	telemetryCfg, err := telemetry.LoadConfig("doctor-ingest")
	if err != nil {
		log.Fatal(err)
	}
	telemetryHandle, err := telemetry.Setup(ctx, telemetryCfg)
	if err != nil {
		log.Fatal(err)
	}
	if telemetryHandle.Enabled() {
		log.SetOutput(io.MultiWriter(os.Stderr, telemetry.NewStdLogWriter("doctor/stdlog")))
		telemetry.EmitInfo(ctx, "doctor/ingest", "doctor ingest telemetry enabled")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown telemetry: %v", err)
			}
		}()
	}
	heartbeatHandle, err := telemetry.RegisterComponentHeartbeat("doctor/ingest")
	if err != nil {
		log.Fatal(err)
	}
	if heartbeatHandle != nil {
		defer func() {
			if err := heartbeatHandle.Unregister(); err != nil {
				log.Printf("unregister ingest heartbeat metrics: %v", err)
			}
		}()
	}
	privacyEngine, err := privacy.New(cfg.Privacy.HMACKey)
	if err != nil {
		log.Fatal(err)
	}

	// Build the writer backend: logengine is the only supported path.
	if cfg.MonoFSLogEngineAddr == "" {
		log.Fatal("DOCTOR_MONOFS_LOGENGINE_ADDR is required (logengine is the only supported storage backend)")
	}
	mw, err := ingest.NewMonoFSWriter(ctx, cfg.MonoFSLogEngineAddr)
	if err != nil {
		log.Fatalf("connect to monofs logengine at %s: %v", cfg.MonoFSLogEngineAddr, err)
	}
	defer mw.Close()
	writer := ingest.Flusher(mw)
	log.Printf("using monofs logengine backend at %s", cfg.MonoFSLogEngineAddr)

	var traceBuffer *ingest.TraceBuffer
	if cfg.TraceBuffer.Enabled {
		traceLog, err := wal.NewTraceBatchLog(cfg.TraceBuffer.WALDir)
		if err != nil {
			log.Fatal(err)
		}
		logWALSize("trace", cfg.TraceBuffer.WALDir+"/trace-batches.log")
		traceBuffer = ingest.NewTraceBuffer(writer, traceLog, ingest.TraceBufferConfig{
			FlushInterval:   cfg.TraceBuffer.FlushInterval,
			MaxRecords:      cfg.TraceBuffer.MaxRecords,
			MaxBytes:        cfg.TraceBuffer.MaxBytes,
			MaxPendingBytes: cfg.TraceBuffer.MaxPendingBytes,
		})
		logMemStats("before trace replay")
		if err := traceBuffer.Replay(ctx); err != nil {
			log.Printf("trace buffer recovery: %v", err)
		}
		logMemStats("after trace replay")
		traceBuffer.Start(ctx)
		defer traceBuffer.Stop(5 * time.Second)
	}

	var logBuffer *ingest.LogBuffer
	if cfg.LogBuffer.Enabled {
		logLog, err := wal.NewLogBatchLog(cfg.LogBuffer.WALDir)
		if err != nil {
			log.Fatal(err)
		}
		logWALSize("log", cfg.LogBuffer.WALDir+"/log-batches.log")
		logBuffer = ingest.NewLogBuffer(writer, logLog, ingest.LogBufferConfig{
			FlushInterval:   cfg.LogBuffer.FlushInterval,
			MaxBytes:        cfg.LogBuffer.MaxBytes,
			MaxPendingBytes: cfg.LogBuffer.MaxPendingBytes,
		})
		logMemStats("before log replay")
		if err := logBuffer.Replay(ctx); err != nil {
			log.Printf("log buffer recovery: %v", err)
		}
		logMemStats("after log replay")
		logBuffer.Start(ctx)
		defer logBuffer.Stop(5 * time.Second)
	}

	var metricBuffer *ingest.MetricBuffer
	if cfg.MetricBuffer.Enabled {
		metricLog, err := wal.NewMetricBatchLog(cfg.MetricBuffer.WALDir)
		if err != nil {
			log.Fatal(err)
		}
		logWALSize("metric", cfg.MetricBuffer.WALDir+"/metric-batches.log")
		metricBuffer = ingest.NewMetricBuffer(writer, metricLog, ingest.MetricBufferConfig{
			FlushInterval:   cfg.MetricBuffer.FlushInterval,
			MaxBytes:        cfg.MetricBuffer.MaxBytes,
			MaxPendingBytes: cfg.MetricBuffer.MaxPendingBytes,
		})
		logMemStats("before metric replay")
		if err := metricBuffer.Replay(ctx); err != nil {
			log.Printf("metric buffer recovery: %v", err)
		}
		logMemStats("after metric replay")
		metricBuffer.Start(ctx)
		defer metricBuffer.Stop(5 * time.Second)
	}

	bufferMetricsHandle, err := ingest.RegisterBufferMetrics(traceBuffer, logBuffer, metricBuffer)
	if err != nil {
		log.Fatal(err)
	}
	if bufferMetricsHandle != nil {
		defer func() {
			if err := bufferMetricsHandle.Unregister(); err != nil {
				log.Printf("unregister ingest buffer metrics: %v", err)
			}
		}()
	}

	var pipeline *ingest.Pipeline
	if cfg.Pipeline.Enabled {
		pipeline = ingest.NewPipeline(writer, ingest.PipelineConfig{
			QueueSize: cfg.Pipeline.QueueSize,
			Workers:   cfg.Pipeline.Workers,
		})
		pipeline.Start(ctx)
		defer pipeline.Stop(5 * time.Second)
	}

	service := ingest.NewServiceWithBuffers(cfg.TenantHeader, cfg.DefaultTenant, writer, pipeline, traceBuffer, logBuffer, metricBuffer, privacyEngine)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		log.Fatal(err)
	}
	grpcServerOptions := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(256 * 1024 * 1024),
		grpc.MaxSendMsgSize(256 * 1024 * 1024),
	}
	grpcServer := grpc.NewServer(grpcServerOptions...)
	if telemetryHandle.Enabled() {
		grpcServer = grpc.NewServer(append(grpcServerOptions, grpc.StatsHandler(otelgrpc.NewServerHandler()))...)
	}
	service.RegisterGRPC(grpcServer)
	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: service.Handler(),
	}
	if telemetryHandle.Enabled() {
		httpServer.Handler = otelhttp.NewHandler(httpServer.Handler, "doctor-ingest-http")
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- grpcServer.Serve(lis)
	}()
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}

	grpcServer.GracefulStop()
	_ = httpServer.Shutdown(context.Background())
}

func logMemStats(label string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("[mem] %s: alloc=%dMiB heap_inuse=%dMiB sys=%dMiB gc_runs=%d",
		label, m.Alloc>>20, m.HeapInuse>>20, m.Sys>>20, m.NumGC)
}

func logWALSize(name, path string) {
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[wal] %s: no WAL file (%v)", name, err)
		return
	}
	log.Printf("[wal] %s: size=%dMiB (%d bytes)", name, info.Size()>>20, info.Size())
}
