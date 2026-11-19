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

	"github.com/radryc/doctor/internal/auditlog"
	"github.com/radryc/doctor/internal/auth"
	"github.com/radryc/doctor/internal/config"
	"github.com/radryc/doctor/internal/quota"
	"github.com/radryc/doctor/internal/ruler"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"github.com/radryc/doctor/internal/telemetry"
	"github.com/radryc/doctor/internal/tlsconfig"
	"github.com/radryc/monofs/pkg/authz"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.LoadRules("doctor-rules")
	if !cfg.Enabled {
		log.Fatal("doctor-rules is disabled (DOCTOR_RULES_ENABLED=false)")
	}
	tlsEnv, err := tlsconfig.Load()
	if err != nil {
		log.Fatalf("load STRATA_TLS_* configuration: %v", err)
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
	if strings.TrimSpace(cfg.APIToken) == "" && strings.TrimSpace(cfg.AuthTokensFile) == "" {
		log.Fatal("DOCTOR_RULES_API_TOKEN or DOCTOR_RULES_TOKENS_FILE is required to authenticate the rules API")
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

	logEngine, err := monofsstore.NewLogEngineClientWithToken(ctx, cfg.MonoFSLogEngineAddr, cfg.MonoFSToken)
	if err != nil {
		log.Fatalf("connect to monofs logengine: %v", err)
	}
	defer logEngine.Close()

	var limits *quota.Provider
	if cfg.Quota.Enabled {
		limits = quota.NewProvider(quota.ProviderOptions{
			PolicyURL:   cfg.Quota.PolicyURL,
			PolicyToken: cfg.Quota.PolicyToken,
			Interval:    cfg.Quota.Interval,
			Defaults:    cfg.Quota.Defaults,
			Enforce:     cfg.Quota.Enforce,
			Logger:      log.Default(),
		})
		go limits.Run(ctx)
		log.Printf("rule quota enabled (policy=%q)", cfg.Quota.PolicyURL)
	}
	store := ruler.NewStore(files, cfg.MonoFSToken, cfg.StoreRoot)
	evaluator := &ruler.Evaluator{
		Querier: ruler.NewHTTPQuerier(cfg.QueryURL, cfg.QueryToken, nil),
		Writer:  ruler.NewLogEngineWriter(logEngine),
		Timeout: cfg.EvalTimeout,
		Logger:  log.Default(),
	}
	scheduler := &ruler.Scheduler{
		Evaluator:       evaluator,
		Load:            store.List,
		Limits:          limits,
		Workers:         cfg.Workers,
		PollInterval:    cfg.PollInterval,
		CatchUp:         cfg.MaxCatchUp,
		MaxCatchUpTicks: cfg.MaxCatchUpTicks,
		Logger:          log.Default(),
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
		Authorizer: ruler.ScopedRulesAuthorizer{},
		Limits:     limits,
		Scheduler:  scheduler,
		Lease:      lease,
		Logger:     log.Default(),
	}
	go lease.Run(ctx, scheduler.Run)

	// TODO(security): accept OIDC identities here (issuer/audience/groups) so
	// partition ownership can come from the IdP instead of a static token file.
	verifiers := make([]authz.TokenVerifier, 0, 2)
	if strings.TrimSpace(cfg.AuthTokensFile) != "" {
		scoped, err := ruler.LoadRulesTokenFile(cfg.AuthTokensFile)
		if err != nil {
			log.Fatalf("load rules token file: %v", err)
		}
		verifiers = append(verifiers, scoped)
		log.Printf("rules token file loaded from %s (%d credentials)", cfg.AuthTokensFile, scoped.Count())
	}
	if strings.TrimSpace(cfg.APIToken) != "" {
		legacy := authz.NewStaticTokenVerifier()
		legacy.Add(cfg.APIToken, authz.Identity{
			Subject:  "doctor-rules-api",
			ClientID: "doctor-rules-api",
			Groups:   []string{ruler.RulesAdminGroup},
		})
		verifiers = append(verifiers, legacy)
		log.Printf("legacy DOCTOR_RULES_API_TOKEN enabled as an admin credential; migrate to DOCTOR_RULES_TOKENS_FILE for per-partition scopes")
	}
	verifier := authz.NewChainVerifier(verifiers...)
	// Authenticate the transport credential first, then replace the service
	// identity with the signed caller identity when doctor-query forwarded one.
	apiHandler := auth.InternalIdentityMiddleware(cfg.InternalIdentitySecret, log.Default())(api.Handler())
	apiHandler = auth.NewMiddleware(verifier, true, []string{"/healthz", "/metrics"}, log.Default()).Wrap(apiHandler)
	apiHandler = auditlog.MutationMiddleware("doctor-rules", "/v1/rules")(apiHandler)
	if strings.TrimSpace(cfg.InternalIdentitySecret) == "" {
		log.Printf("DOCTOR_INTERNAL_IDENTITY_SECRET is unset: proxied user identities will fall back to the rules service credential")
	}

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
	serverTLS, err := tlsEnv.ServerTLSHTTP(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		log.Fatalf("configure HTTP TLS: %v", err)
	}
	server.TLSConfig = serverTLS
	log.Printf("doctor-rules listening on %s (ruler=%s, store=%s, tls=%t, tls_mode=%s)", cfg.HTTPAddr, cfg.RulerID, cfg.StoreRoot, serverTLS != nil, tlsEnv.Mode())
	var serveErr error
	if serverTLS != nil {
		serveErr = server.ListenAndServeTLS("", "")
	} else {
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatal(serveErr)
	}
}
