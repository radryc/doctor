package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/doctor/internal/auth"
	"github.com/radryc/doctor/internal/config"
	"github.com/radryc/doctor/internal/dashboards"
	"github.com/radryc/doctor/internal/federation"
	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/query"
	"github.com/radryc/doctor/internal/ruler"
	awsstore "github.com/radryc/doctor/internal/store/aws"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"github.com/radryc/doctor/internal/telemetry"
	"github.com/radryc/monofs/pkg/authz"
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
		log.SetOutput(io.MultiWriter(os.Stderr, telemetry.NewStdLogWriter("doctor.stdlog")))
		telemetry.EmitInfo(ctx, "doctor.query", "doctor query telemetry enabled")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown telemetry: %v", err)
			}
		}()
	}
	heartbeatHandle, err := telemetry.RegisterComponentHeartbeat("doctor.query")
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
	svc.SetTraceDiscoveryCacheTTL(cfg.TraceDiscoveryCacheTTL)
	svc.SetTraceDiscoveryWindow(cfg.TraceDiscoveryWindow)
	svc.SetTenantHeader(cfg.TenantHeader)
	svc.SetGuardianURL(cfg.GuardianURL)
	svc.SetGuardianAPIURL(cfg.GuardianAPIURL)
	svc.SetDefaultPartition(cfg.DefaultPartition)
	svc.SetRulesURL(cfg.RulesURL)
	svc.SetRulesToken(cfg.RulesAPIToken)
	if strings.TrimSpace(cfg.RulesURL) != "" && strings.TrimSpace(cfg.RulesAPIToken) == "" {
		log.Printf("rules proxy is configured without DOCTOR_RULES_API_TOKEN; doctor-rules requires it and proxy requests will fail")
	}
	if strings.TrimSpace(cfg.RulesURL) != "" {
		log.Printf("recording rules UI enabled via %s", cfg.RulesURL)
	}
	svc.SetSentinelURL(cfg.SentinelURL)
	if strings.TrimSpace(cfg.SentinelURL) != "" {
		log.Printf("sentinel UI enabled via %s", cfg.SentinelURL)
	}

	// Shared CAS file store: MonoFS when a token is set, in-memory fallback for
	// local dev. Dashboards, recording rules and the federation catalog share
	// the same file-store implementation.
	var sharedFS ruler.FileStore
	{
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
				log.Fatalf("dashboards: connect MonoFS file store: %v", err)
			}
			defer dashClient.Close()
			sharedFS = dashClient
			log.Printf("file store: using MonoFS at %s", cfg.MonoFSLogEngineAddr)
		} else {
			sharedFS = ruler.NewMemoryStore()
			log.Printf("file store: DOCTOR_MONOFS_TOKEN unset — using in-memory store")
		}
		dashStore := dashboards.NewStore(sharedFS, cfg.MonoFSFileToken, cfg.DashboardsStoreRoot)
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

	federationTokens, tokenSource := buildFederationTokenResolver(cfg, sharedFS)
	if federationTokens != nil {
		log.Printf("federation token resolver enabled: %s", tokenSource)
	}

	var awsClient proxy.AWSClient
	switch {
	case cfg.FederationEnabled:
		catalog, source, catalogStore, catalogErr := loadFederationCatalog(ctx, cfg, sharedFS)
		if catalogErr != nil {
			if errors.Is(catalogErr, federation.ErrSnapshotNotFound) && cfg.AWSEnabled && cfg.AWSBackendURL != "" {
				log.Printf("aws federation catalog not found (%v); falling back to legacy single backend %s", catalogErr, cfg.AWSBackendURL)
				legacy := awsstore.NewClient(cfg.AWSBackendURL, cfg.AWSBackendToken)
				awsClient = legacy
				svc.SetAWSBackend(legacy)
			} else {
				log.Fatalf("load aws federation catalog: %v", catalogErr)
			}
			break
		}
		manager := federation.NewManager(catalog, federation.Options{
			MaxFanout:            cfg.FederationMaxFanout,
			MaxInflight:          cfg.FederationMaxInflight,
			MaxConcurrentTargets: cfg.FederationMaxConcurrentTargets,
			MaxAttempts:          cfg.FederationMaxAttempts,
			ClientPoolMax:        cfg.FederationClientPoolMax,
			ClientIdleTTL:        cfg.FederationClientIdleTTL,
			BreakerThreshold:     cfg.FederationBreakerThreshold,
			BreakerCooldown:      cfg.FederationBreakerCooldown,
			MetadataTTL:          cfg.FederationMetadataTTL,
			Timeout:              cfg.FederationTimeout,
			StaleAfter:           cfg.FederationStaleAfter,
			Source:               source,
			TokenResolver:        federationTokens,
		})
		awsClient = manager
		svc.SetAWSBackend(manager)
		catalogMetrics, metricsErr := federation.RegisterCatalogMetrics(manager)
		if metricsErr != nil {
			log.Printf("federation catalog metrics disabled: %v", metricsErr)
		} else {
			manager.SetMetrics(catalogMetrics)
			defer func() { _ = catalogMetrics.Unregister() }()
		}
		if catalogStore != nil && cfg.FederationReloadInterval > 0 {
			reloader := federation.NewCatalogReloader(
				catalogStore,
				manager,
				federationReloadSource(cfg, source),
				cfg.FederationReloadInterval,
				log.Default(),
			)
			reloader.Metrics = catalogMetrics
			go reloader.Run(ctx)
			log.Printf("federation catalog reload enabled every %s", cfg.FederationReloadInterval)
		}
		go manager.Run(ctx)
		log.Printf("multi-account aws federation enabled with %d target(s) from %s", catalog.Len(), source)
	case cfg.AWSEnabled && cfg.AWSBackendURL != "":
		legacy := awsstore.NewClient(cfg.AWSBackendURL, cfg.AWSBackendToken)
		awsClient = legacy
		svc.SetAWSBackend(legacy)
		log.Printf("aws federation enabled via %s (legacy single backend)", cfg.AWSBackendURL)
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
		if awsClient != nil {
			svc.SetAWSViaRouter(true)
		}
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
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	if telemetryHandle.Enabled() {
		server.Handler = otelhttp.NewHandler(server.Handler, "doctor-query-http")
	}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
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
func setupProxyRouter(ctx context.Context, cfg config.QueryConfig, svc *query.Service, awsClient proxy.AWSClient, gate *access.Gate) {
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
		federated := false
		origin := "config"
		if _, ok := awsClient.(*federation.Manager); ok {
			federated = true
			origin = "aws-federated"
		}
		awsBackend := proxy.Backend{
			Name:       "aws",
			Kind:       proxy.KindAWS,
			Signals:    []proxy.Signal{proxy.SignalMetrics, proxy.SignalLogs, proxy.SignalTraces},
			Policy:     proxy.PolicyFallback,
			OwnersPath: "/doctor/v1/endpoints/aws",
			Origin:     origin,
			Federated:  federated,
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

// buildFederationTokenResolver selects the token source for catalog targets.
// The default is Guardian (lazy, one pusher at a time), with a sharded MonoFS
// bundle as a fallback and env only for development/static catalogs.
func buildFederationTokenResolver(cfg config.QueryConfig, files ruler.FileStore) (federation.TokenResolver, string) {
	mode := strings.ToLower(strings.TrimSpace(cfg.FederationTokenResolver))
	if mode == "" {
		mode = "auto"
	}
	guardianToken := strings.TrimSpace(cfg.GuardianToken)
	if guardianToken == "" {
		guardianToken = strings.TrimSpace(cfg.AccessPolicyToken)
	}
	guardianURL := strings.TrimSpace(cfg.GuardianAPIURL)
	if guardianURL == "" {
		guardianURL = strings.TrimSpace(cfg.GuardianURL)
	}

	var guardian federation.TokenResolver
	if guardianURL != "" {
		guardian = federation.NewGuardianTokenResolver(federation.GuardianTokenResolverOptions{
			BaseURL:  guardianURL,
			Token:    guardianToken,
			CacheTTL: cfg.FederationTokenCacheTTL,
		})
	}
	var bundle federation.TokenResolver
	if files != nil && strings.TrimSpace(cfg.MonoFSFileToken) != "" {
		bundleResolver := federation.NewBundleTokenResolver(federation.BundleTokenResolverOptions{
			Files:      files,
			Root:       cfg.FederationTokenBundleRoot,
			Tenant:     cfg.FederationSnapshotTenant,
			ShardCount: cfg.FederationTokenShardCount,
			CacheTTL:   cfg.FederationTokenCacheTTL,
		})
		bundleResolver.Key = []byte(cfg.FederationTokenKey)
		bundle = bundleResolver
	}

	switch mode {
	case "none", "off", "disabled":
		return nil, "disabled"
	case "env":
		return federation.EnvTokenResolver{}, "env"
	case "guardian":
		if guardian == nil {
			log.Printf("federation token resolver: guardian requested but no Guardian API URL is configured")
			return nil, "guardian (unconfigured)"
		}
		return guardian, "guardian"
	case "bundle", "monofs":
		if bundle == nil {
			log.Printf("federation token resolver: bundle requested but MonoFS file store is not configured")
			return nil, "bundle (unconfigured)"
		}
		return bundle, "monofs bundle"
	case "auto":
		chain := federation.ChainTokenResolver{federation.EnvTokenResolver{}}
		sources := "env"
		if guardian != nil {
			chain = append(chain, guardian)
			sources += ",guardian"
		}
		if bundle != nil {
			chain = append(chain, bundle)
			sources += ",monofs-bundle"
		}
		return chain, sources
	default:
		log.Printf("federation token resolver: unknown mode %q, using auto", mode)
		chain := federation.ChainTokenResolver{federation.EnvTokenResolver{}}
		sources := "env"
		if guardian != nil {
			chain = append(chain, guardian)
			sources += ",guardian"
		}
		if bundle != nil {
			chain = append(chain, bundle)
			sources += ",monofs-bundle"
		}
		return chain, sources
	}
}

// loadFederationCatalog loads the catalog from the configured snapshot store.
// MonoFS is preferred when configured; a static file remains the bootstrap and
// local-development fallback.
func loadFederationCatalog(ctx context.Context, cfg config.QueryConfig, files ruler.FileStore) (*federation.Catalog, string, federation.SnapshotStore, error) {
	// AWS ECS deployments have no ConfigMap/volume in the first deployment
	// slice. Accept the same snapshot document inline so Doctor can run with
	// all desired state carried as an encrypted task-definition environment
	// value.
	if raw := strings.TrimSpace(cfg.FederationCatalogJSON); raw != "" {
		snapshot, err := federation.ParseSnapshot([]byte(raw))
		if err != nil {
			return nil, "", nil, fmt.Errorf("parse inline federation catalog: %w", err)
		}
		catalog, err := federation.NewCatalog(snapshot.Targets, snapshot.Version)
		if err != nil {
			return nil, "", nil, err
		}
		source := "inline"
		if version := strings.TrimSpace(snapshot.Version); version != "" {
			source += ":" + version
		}
		return catalog, source, nil, nil
	}

	storeName := strings.ToLower(strings.TrimSpace(cfg.FederationSnapshotStore))
	switch storeName {
	case "", "file":
		// Use the static file below.
	case "monofs":
		if files == nil || strings.TrimSpace(cfg.MonoFSFileToken) == "" {
			return nil, "", nil, fmt.Errorf("monofs federation snapshot store requires DOCTOR_MONOFS_TOKEN")
		}
		snapshotStore := federation.NewMonoFSSnapshotStore(
			files,
			cfg.MonoFSFileToken,
			cfg.FederationSnapshotRoot,
			cfg.FederationSnapshotTenant,
		)
		snapshotStore.ShardTargets = cfg.FederationSnapshotShardTargets
		snapshot, err := snapshotStore.Load(ctx)
		if err == nil {
			catalog, catalogErr := federation.NewCatalog(snapshot.Targets, snapshot.Version)
			return catalog, "monofs:" + snapshot.Version, snapshotStore, catalogErr
		}
		if !errors.Is(err, federation.ErrSnapshotNotFound) {
			return nil, "", nil, err
		}
		// No snapshot yet: fall through to the static bootstrap file.
	default:
		return nil, "", nil, fmt.Errorf("unknown DOCTOR_FEDERATION_SNAPSHOT_STORE %q", storeName)
	}

	if strings.TrimSpace(cfg.FederationCatalogFile) == "" {
		return nil, "", nil, federation.ErrSnapshotNotFound
	}
	snapshotStore := &federation.FileSnapshotStore{Path: cfg.FederationCatalogFile}
	snapshot, err := snapshotStore.Load(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	catalog, err := federation.NewCatalog(snapshot.Targets, snapshot.Version)
	if err != nil {
		return nil, "", nil, err
	}
	return catalog, cfg.FederationCatalogFile, snapshotStore, nil
}

func federationReloadSource(cfg config.QueryConfig, source string) string {
	if strings.HasPrefix(source, "monofs:") {
		return "monofs"
	}
	if strings.TrimSpace(cfg.FederationCatalogFile) != "" {
		return "file"
	}
	return "catalog"
}
