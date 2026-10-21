package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/rydzu/ainfra/doctor/internal/access"
	"github.com/rydzu/ainfra/doctor/internal/auth"
	"github.com/rydzu/ainfra/doctor/internal/config"
	"github.com/rydzu/ainfra/doctor/internal/dashboards"
	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/query"
	"github.com/rydzu/ainfra/doctor/internal/ruler"
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
	svc.SetRulesURL(cfg.RulesURL)
	if strings.TrimSpace(cfg.RulesURL) != "" {
		log.Printf("recording rules UI enabled via %s", cfg.RulesURL)
	}

	// Shared Dashboards Store: MonoFS CAS when token is set, in-memory fallback for local dev.
	{
		var dashFS ruler.FileStore
		if strings.TrimSpace(cfg.MonoFSFileToken) != "" {
			dashClient, err := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
				RouterAddr:  cfg.MonoFSLogEngineAddr,
				ClientID:    cfg.MonoFSClientID,
				Token:       cfg.MonoFSFileToken,
				PrincipalID: "doctor-query",
				Role:        "doctor",
				Writable:    true,
			})
			if err != nil {
				log.Printf("dashboards: could not connect MonoFS file store: %v — using in-memory store", err)
				dashFS = ruler.NewMemoryStore()
			} else {
				defer dashClient.Close()
				dashFS = dashClient
				log.Printf("dashboards: using MonoFS store at %s root %s", cfg.MonoFSLogEngineAddr, cfg.DashboardsStoreRoot)
			}
		} else {
			dashFS = ruler.NewMemoryStore()
			log.Printf("dashboards: DOCTOR_MONOFS_TOKEN unset — using in-memory store (presets always available)")
		}
		dashStore := dashboards.NewStore(dashFS, cfg.MonoFSFileToken, cfg.DashboardsStoreRoot)
		svc.SetDashboardStore(dashStore)
	}

	// Repo-managed access policy: filter logs and traces per partition. The
	// gate is nil (feature disabled) unless a policy URL or file is set.
	policyGate := access.NewPolicyGate(access.PolicyOptions{
		URL:      cfg.AccessPolicyURL,
		File:     cfg.AccessPolicyFile,
		Token:    cfg.AccessPolicyToken,
		Interval: cfg.AccessPolicyInterval,
		Enforce:  strings.EqualFold(strings.TrimSpace(cfg.AccessMode), "enforce"),
		Logger:   log.Default(),
	})
	if policyGate != nil {
		go policyGate.Run(ctx)
		svc.SetPolicyGate(policyGate)
		log.Printf("access policy enabled (url=%q file=%q enforce=%v)",
			cfg.AccessPolicyURL, cfg.AccessPolicyFile, policyGate.Enforced())
	}

	var awsClient *awsstore.Client
	if cfg.AWSEnabled && cfg.AWSBackendURL != "" {
		awsClient = awsstore.NewClient(cfg.AWSBackendURL, cfg.AWSBackendToken)
		svc.SetAWSBackend(awsClient)
		log.Printf("aws federation enabled via %s", cfg.AWSBackendURL)
	}

	gate, authMiddleware, webAuth, cleanup, err := setupSecurity(ctx, cfg)
	if err != nil {
		log.Fatalf("setup security: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if cfg.ProxyEnabled {
		setupProxyRouter(ctx, cfg, svc, awsClient, gate)
	}

	handler := svc.Handler()
	// Request-level policy gate runs inside auth so identity is available;
	// record-level filtering lives in the backend decorator.
	if policyGate != nil {
		handler = policyGate.Middleware(handler)
	}
	handler = authMiddleware.Wrap(handler)
	if cfg.AccessTrustTenantHeader {
		trust := auth.NewTenantTrust(cfg.TenantHeader, log.Default())
		handler = trust.Wrap(handler)
		log.Printf("access: trusting %s header as machine identity tenant:<name> (group doctor:tenant)", cfg.TenantHeader)
	}
	if webAuth != nil {
		handler = webAuth.Handler(handler)
	}

	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
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

// setupSecurity builds the authentication middleware and the access gate.
// It returns a cleanup function for resources that must be released at
// shutdown.
func setupSecurity(ctx context.Context, cfg config.QueryConfig) (*access.Gate, *auth.Middleware, *authz.WebAuthenticator, func(), error) {
	authCfg := auth.Config{
		Issuer:          cfg.OIDCIssuer,
		Audience:        cfg.OIDCAudience,
		JWKSURL:         cfg.OIDCJWKSURL,
		ClientID:        cfg.OIDCClientID,
		ClientSecret:    cfg.OIDCClientSecret,
		RedirectURL:     cfg.OIDCRedirectURL,
		EndSessionURL:   cfg.OIDCEndSessionURL,
		AuthURL:         cfg.OIDCAuthURL,
		Scopes:          cfg.OIDCScopes,
		SessionDir:      cfg.SessionDir,
		BreakGlassToken: cfg.BreakGlassToken,
		Require:         cfg.AuthRequire,
		RequireLogin:    cfg.OIDCRequireLogin,
		ExemptPaths:     cfg.AuthExemptPaths,
	}
	verifier, err := auth.NewVerifier(authCfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if verifier != nil {
		log.Printf("authentication enabled (issuer=%q require=%v)", cfg.OIDCIssuer, cfg.AuthRequire)
	} else {
		log.Printf("authentication disabled: set DOCTOR_OIDC_ISSUER/AUDIENCE or DOCTOR_BREAK_GLASS_TOKEN")
	}
	middleware := auth.NewMiddleware(verifier, cfg.AuthRequire, cfg.AuthExemptPaths, log.Default())

	webAuth, err := auth.NewWebAuthenticator(ctx, authCfg, verifier)
	if err != nil {
		log.Printf("browser login disabled: %v", err)
		webAuth = nil
	} else if webAuth != nil {
		log.Printf("browser login enabled (client=%q)", cfg.OIDCClientID)
	}

	resolver, cleanup, err := buildAccessResolver(ctx, cfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	enforce := map[access.Signal]bool{}
	if strings.EqualFold(cfg.AccessMode, "enforce") {
		enforce[access.SignalLogs] = true
		enforce[access.SignalTraces] = true
	} else {
		enforce[access.SignalLogs] = cfg.AccessEnforceLogs
		enforce[access.SignalTraces] = cfg.AccessEnforceTraces
	}
	gate := access.NewGate(resolver, enforce, log.Default(), nil)
	log.Printf("access resolver=%s mode=%s enforceLogs=%v enforceTraces=%v",
		resolver.ResolverName(), cfg.AccessMode, enforce[access.SignalLogs], enforce[access.SignalTraces])
	return gate, middleware, webAuth, cleanup, nil
}

func buildAccessResolver(ctx context.Context, cfg config.QueryConfig) (access.Resolver, func(), error) {
	static := access.NewStatic(cfg.AccessStaticAllow, cfg.AccessStaticAllowGroups, cfg.AccessStaticDeny)
	grantsResolver := func() (access.Resolver, error) {
		store, err := authz.NewGrantStore(cfg.AccessGrantsPath)
		if err != nil {
			return nil, err
		}
		return &access.Grants{Store: store}, nil
	}
	ownersResolver := func() (access.Resolver, func(), error) {
		mapping, err := loadTeamMapping(cfg.AccessTeamMapping)
		if err != nil {
			return nil, nil, err
		}
		var source access.OwnersSource
		cleanup := func() {}
		if strings.TrimSpace(cfg.MonoFSFileToken) != "" {
			client, clientErr := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
				RouterAddr: cfg.MonoFSLogEngineAddr,
				ClientID:   cfg.MonoFSClientID,
				Token:      cfg.MonoFSFileToken,
				Role:       "doctor",
			})
			if clientErr != nil {
				return nil, nil, clientErr
			}
			source = &access.MonoFSSource{Reader: client}
			cleanup = func() { _ = client.Close() }
		}
		return access.NewOwners(source, mapping, cfg.AccessOwnersRoot, cfg.AccessCacheTTL), cleanup, nil
	}

	switch strings.ToLower(strings.TrimSpace(cfg.AccessResolver)) {
	case "allow-all":
		return access.AllowAll{}, nil, nil
	case "static":
		return static, nil, nil
	case "grants":
		resolver, err := grantsResolver()
		return resolver, nil, err
	case "chain":
		owners, cleanup, err := ownersResolver()
		if err != nil {
			return nil, nil, err
		}
		grants, err := grantsResolver()
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		return access.NewChain(owners, grants, static), cleanup, nil
	default:
		resolver, cleanup, err := ownersResolver()
		return resolver, cleanup, err
	}
}

func loadTeamMapping(raw string) (authz.TeamMapping, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		data = []byte(raw)
	}
	return authz.ParseTeamMapping(data)
}

// setupProxyRouter wires the multi-backend registry and router: local MonoFS
// storage is always the "doctor" backend, AWS is registered with the legacy
// fallback policy, and additional backends come from the static file and
// Guardian discovery.
func setupProxyRouter(ctx context.Context, cfg config.QueryConfig, svc *query.Service, awsClient *awsstore.Client, gate *access.Gate) {
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
		Access:         gate,
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
		Traces:  svc.LocalTracesUpstream(),
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
		awsUpstream := svc.AWSUpstream()
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
