package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/config"
	"github.com/rydzu/ainfra/doctor/internal/query"
	monofsstore "github.com/rydzu/ainfra/doctor/internal/store/monofs"
	"github.com/rydzu/ainfra/doctor/internal/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.LoadQuery("doctor-query")
	telemetryCfg, err := telemetry.LoadConfig("doctor-query")
	if err != nil {
		log.Fatal(err)
	}
	telemetryHandle, err := telemetry.Setup(ctx, telemetryCfg)
	if err != nil {
		log.Fatal(err)
	}
	if telemetryHandle.Enabled() {
		log.SetOutput(io.MultiWriter(os.Stderr, telemetry.NewStdLogWriter("doctor/stdlog")))
		telemetry.EmitInfo(ctx, "doctor/query", "doctor query telemetry enabled")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown telemetry: %v", err)
			}
		}()
	}
	heartbeatHandle, err := telemetry.RegisterComponentHeartbeat("doctor/query")
	if err != nil {
		log.Fatal(err)
	}
	if heartbeatHandle != nil {
		defer func() {
			if err := heartbeatHandle.Unregister(); err != nil {
				log.Printf("unregister query heartbeat metrics: %v", err)
			}
		}()
	}

	if cfg.MonoFSLogEngineAddr == "" {
		log.Fatal("DOCTOR_MONOFS_LOGENGINE_ADDR is required (logengine is the only supported storage backend)")
	}

	leClient, err := monofsstore.NewLogEngineClient(ctx, cfg.MonoFSLogEngineAddr)
	if err != nil {
		log.Fatalf("connect to monofs logengine at %s: %v", cfg.MonoFSLogEngineAddr, err)
	}
	defer leClient.Close()
	log.Printf("using monofs logengine backend at %s", cfg.MonoFSLogEngineAddr)

	svc := query.NewServiceLogEngine(leClient, cfg.DefaultTenant)
	svc.SetGuardianURL(cfg.GuardianURL)
	svc.SetGuardianAPIURL(cfg.GuardianAPIURL)
	svc.SetDefaultPartition(cfg.DefaultPartition)

	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: svc.Handler(),
	}
	if telemetryHandle.Enabled() {
		server.Handler = otelhttp.NewHandler(server.Handler, "doctor-query-http")
	}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
