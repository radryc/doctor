package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/doctor/internal/auth"
	"github.com/radryc/doctor/internal/config"
	"github.com/radryc/doctor/internal/ruler"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"github.com/radryc/doctor/internal/telemetry"
	"github.com/radryc/monofs/pkg/authz"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.LoadRules("doctor-rules")
	if !cfg.Enabled {
		log.Fatal("doctor-rules is disabled (DOCTOR_RULES_ENABLED=false)")
	}
	telemetryCfg, err := telemetry.LoadConfig("doctor-rules")
	if err != nil {
		log.Fatal(err)
	}
	telemetryHandle, err := telemetry.Setup(ctx, telemetryCfg)
	if err != nil {
		log.Fatal(err)
	}
	if telemetryHandle.Enabled() {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = telemetryHandle.Shutdown(shutdownCtx)
		}()
	}
	heartbeatHandle, err := telemetry.RegisterComponentHeartbeat("doctor.rules")
	if err != nil {
		log.Fatal(err)
	}
	if heartbeatHandle != nil {
		defer func() { _ = heartbeatHandle.Unregister() }()
	}
	if cfg.MonoFSLogEngineAddr == "" {
		log.Fatal("DOCTOR_MONOFS_LOGENGINE_ADDR is required")
	}
	if cfg.MonoFSToken == "" {
		log.Fatal("DOCTOR_MONOFS_TOKEN is required to read and write rules in MonoFS")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		log.Fatal("DOCTOR_RULES_API_TOKEN is required to authenticate the rules API")
	}

	files, err := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
		RouterAddr: cfg.MonoFSLogEngineAddr,
		ClientID:   cfg.MonoFSClientID,
		Token:      cfg.MonoFSToken,
		Role:       "doctor",
	})
	if err != nil {
		log.Fatalf("connect to monofs: %v", err)
	}
	defer func() { _ = files.Close() }()

	logEngine, err := monofsstore.NewLogEngineClient(ctx, cfg.MonoFSLogEngineAddr)
	if err != nil {
		log.Fatalf("connect to monofs logengine: %v", err)
	}
	defer logEngine.Close()

	store := ruler.NewStore(files, cfg.MonoFSToken, cfg.StoreRoot)
	evaluator := &ruler.Evaluator{
		Querier: ruler.NewHTTPQuerier(cfg.QueryURL, cfg.QueryToken, nil),
		Writer:  ruler.NewLogEngineWriter(logEngine),
		Timeout: cfg.EvalTimeout,
		Logger:  log.Default(),
	}
	scheduler := &ruler.Scheduler{
		Evaluator:    evaluator,
		Load:         store.List,
		Workers:      cfg.Workers,
		PollInterval: cfg.PollInterval,
		Logger:       log.Default(),
	}

	lease := ruler.NewLeaseManager(
		&ruler.FileLeaseStore{Files: files, Token: cfg.MonoFSToken},
		cfg.StoreRoot+"/.lease/leader.json",
		cfg.RulerID,
		cfg.LeaseTTL,
		cfg.LeaseRenew,
		log.Default(),
	)
	api := &ruler.API{
		Store:      store,
		Evaluator:  evaluator,
		Authorizer: ruler.AuthenticatedAuthorizer{},
		Scheduler:  scheduler,
		Lease:      lease,
		Logger:     log.Default(),
	}
	go lease.Run(ctx, scheduler.Run)

	verifier := authz.NewStaticTokenVerifier()
	verifier.Add(cfg.APIToken, authz.Identity{Subject: "doctor-rules-api", ClientID: "doctor-rules-api"})
	apiHandler := auth.NewMiddleware(verifier, true, []string{"/healthz", "/metrics"}, log.Default()).Wrap(api.Handler())

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           apiHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("doctor-rules listening on %s (ruler=%s, store=%s)", cfg.HTTPAddr, cfg.RulerID, cfg.StoreRoot)
	var serveErr error
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		serveErr = server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatal(serveErr)
	}
}
