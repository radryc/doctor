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
	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/query"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
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
	svc.SetTenantHeader(cfg.TenantHeader)
	svc.SetGuardianURL(cfg.GuardianURL)
	svc.SetGuardianAPIURL(cfg.GuardianAPIURL)
	svc.SetDefaultPartition(cfg.DefaultPartition)

	var awsClient *awsstore.Client
	if cfg.AWSEnabled && cfg.AWSBackendURL != "" {
		awsClient = awsstore.NewClient(cfg.AWSBackendURL, cfg.AWSBackendToken)
		svc.SetAWSBackend(awsClient)
		log.Printf("aws federation enabled via %s", cfg.AWSBackendURL)
	}

	if cfg.ProxyEnabled {
		setupProxyRouter(ctx, cfg, svc, awsClient)
	}

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

// setupProxyRouter wires the multi-backend registry and router: local MonoFS
// storage is always the "doctor" backend, AWS is registered with the legacy
// fallback policy, and additional backends come from the static file and
// Guardian discovery.
func setupProxyRouter(ctx context.Context, cfg config.QueryConfig, svc *query.Service, awsClient *awsstore.Client) {
	registries := make([]proxy.Registry, 0, 2)
	if cfg.ProxyBackendsFile != "" {
		static, err := proxy.NewStaticRegistry(proxy.StaticRegistryConfig{
			Path:     cfg.ProxyBackendsFile,
			Interval: cfg.ProxyRegistryInterval,
			Logger:   log.Default(),
		})
		if err != nil {
			log.Fatalf("load proxy backends: %v", err)
		}
		registries = append(registries, static)
		go static.Run(ctx)
		log.Printf("proxy backends loaded from %s", cfg.ProxyBackendsFile)
	}
	if cfg.GuardianDiscoveryEnabled {
		if cfg.GuardianAPIURL == "" {
			log.Printf("guardian discovery enabled but DOCTOR_GUARDIAN_API_URL is unset; discovery disabled")
		} else {
			discovery := proxy.NewGuardianDiscovery(proxy.GuardianDiscoveryConfig{
				BaseURL:  cfg.GuardianAPIURL,
				Token:    cfg.GuardianToken,
				Interval: cfg.GuardianDiscoveryInterval,
				Logger:   log.Default(),
			})
			registries = append(registries, discovery)
			go discovery.Run(ctx)
			log.Printf("guardian backend discovery enabled via %s", cfg.GuardianAPIURL)
		}
	}

	router := proxy.NewRouter(proxy.RouterConfig{
		MaxInflight:    cfg.ProxyMaxInflight,
		BackendTimeout: cfg.ProxyBackendTimeout,
		Logger:         log.Default(),
	}, proxy.NewCompositeRegistry(registries...))

	localBackend := proxy.Backend{
		Name:    "doctor",
		Kind:    proxy.KindMonoFS,
		Signals: []proxy.Signal{proxy.SignalMetrics, proxy.SignalLogs, proxy.SignalTraces},
		Policy:  proxy.PolicyAlways,
		Origin:  "local",
	}
	if err := router.Register(localBackend, proxy.Upstreams{
		Metrics: svc.LocalMetricsUpstream(),
		Logs:    svc.LocalLogsUpstream(),
	}); err != nil {
		log.Fatalf("register local proxy backend: %v", err)
	}
	if awsClient != nil {
		awsBackend := proxy.Backend{
			Name:       "aws",
			Kind:       proxy.KindAWS,
			Signals:    []proxy.Signal{proxy.SignalMetrics, proxy.SignalLogs, proxy.SignalTraces},
			Policy:     proxy.PolicyFallback,
			OwnersPath: "/doctor/v1/endpoints/aws",
			Origin:     "config",
		}
		awsUpstream := &proxy.AWSUpstream{Client: awsClient}
		if err := router.Register(awsBackend, proxy.Upstreams{
			Metrics: awsUpstream,
			Logs:    awsUpstream,
			Traces:  awsUpstream,
		}); err != nil {
			log.Fatalf("register aws proxy backend: %v", err)
		}
	}
	svc.SetProxyRouter(router)
	log.Printf("proxy router enabled (max inflight %d, backend timeout %s)", cfg.ProxyMaxInflight, cfg.ProxyBackendTimeout)
}
