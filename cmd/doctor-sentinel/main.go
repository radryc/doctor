package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/doctor/internal/alerting"
	"github.com/radryc/doctor/internal/auditlog"
	"github.com/radryc/doctor/internal/config"
	"github.com/radryc/doctor/internal/delivery"
	"github.com/radryc/doctor/internal/incident"
	"github.com/radryc/doctor/internal/netguard"
	"github.com/radryc/doctor/internal/ruler"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"github.com/radryc/doctor/internal/tlsconfig"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.LoadSentinel("doctor-sentinel")
	if !cfg.Enabled {
		log.Fatal("doctor-sentinel is disabled (DOCTOR_SENTINEL_ENABLED=false)")
	}
	tlsEnv, err := tlsconfig.Load()
	if err != nil {
		log.Fatalf("load STRATA_TLS_* configuration: %v", err)
	}

	var files alerting.FileStore
	if cfg.MonoFSLogEngineAddr != "" && cfg.MonoFSToken != "" {
		client, err := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
			RouterAddr: cfg.MonoFSLogEngineAddr,
			ClientID:   cfg.MonoFSClientID,
			Token:      cfg.MonoFSToken,
			Role:       "doctor",
			Writable:   true,
		})
		if err != nil {
			log.Fatalf("connect to monofs: %v", err)
		}
		defer func() { _ = client.Close() }()
		files = client
		log.Printf("sentinel store: MonoFS %s", cfg.MonoFSLogEngineAddr)
	} else {
		files = alerting.NewMemoryStore()
		log.Printf("sentinel store: in-memory (set DOCTOR_MONOFS_LOGENGINE_ADDR and DOCTOR_MONOFS_TOKEN for MonoFS)")
	}

	store := alerting.NewStore(files, cfg.MonoFSToken, cfg.StoreRoot)
	alertWake := make(chan struct{}, 1)
	service := &alerting.Service{
		Store:           store,
		DefaultTenant:   cfg.DefaultTenant,
		GuardianHMACKey: []byte(cfg.GuardianHMACSecret),
		DeliveryEnabled: cfg.DeliveryEnabled,
		Logger:          log.Default(),
		Wake: func() {
			select {
			case alertWake <- struct{}{}:
			default:
			}
		},
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = service.ExpireSubspace(ctx, cfg.DefaultTenant)
			}
		}
	}()

	if cfg.QueryURL != "" {
		service.Querier = ruler.NewHTTPQuerier(cfg.QueryURL, cfg.QueryToken, nil)
		scheduler := &alerting.RuleScheduler{
			Load: func(ctx context.Context) ([]alerting.RuleGroupRef, error) {
				return store.ListRuleGroups(ctx, "")
			},
			Evaluate:   service.EvaluateRuleGroup,
			Interval:   cfg.PollInterval,
			ShardIndex: cfg.ShardIndex,
			ShardCount: cfg.EvalShards,
			Logger:     log.Default(),
		}
		lease := alerting.NewLeaseManager(
			files, cfg.MonoFSToken,
			path.Join(cfg.StoreRoot, ".leases", fmt.Sprintf("shard-%d.json", cfg.ShardIndex)),
			cfg.RulerID, cfg.ShardIndex, cfg.LeaseTTL, cfg.LeaseRenew, log.Default(),
		)
		go lease.Run(ctx, scheduler.Run)
		log.Printf("sentinel rule evaluation enabled via %s (shard %d/%d)", cfg.QueryURL, cfg.ShardIndex, cfg.EvalShards)
	}
	api := &alerting.API{
		Store:              store,
		Service:            service,
		Logger:             log.Default(),
		AllowAll:           cfg.AuthAllowAll,
		ServiceToken:       cfg.ServiceToken,
		IdentityHMACSecret: cfg.IdentityHMACSecret,
		FCBSecret:          []byte(cfg.FCBAckSecret),
	}

	urlPolicy := &netguard.Policy{
		RequireHTTPS:  cfg.WebhookRequireHTTPS,
		AllowPrivate:  cfg.WebhookAllowPrivate,
		AllowLoopback: cfg.WebhookAllowLoopback,
	}
	dispatcher := &alerting.Dispatcher{
		Store:     store,
		Logger:    log.Default(),
		URLPolicy: urlPolicy,
		FCB: delivery.FCBConfig{
			Enabled:       cfg.FCBEnabled,
			BackendURL:    cfg.FCBBackendURL,
			WebhookSecret: cfg.FCBWebhookSecret,
			DefaultTopic:  cfg.FCBDefaultTopic,
			Timeout:       cfg.FCBTimeout,
		},
	}
	if cfg.DeliveryEnabled {
		go dispatcher.Run(ctx)
	}
	if cfg.FCBEnabled {
		if cfg.FCBBackendURL == "" {
			log.Printf("sentinel fcb notifications enabled but DOCTOR_SENTINEL_FCB_BACKEND_URL is empty; fcb receivers without urlEnv will fail")
		} else {
			log.Printf("sentinel fcb notifications enabled: %s", cfg.FCBBackendURL)
		}
	}

	var incidentHandler http.Handler
	if cfg.IncidentsEnabled {
		incidentStore := incident.NewStore(files, store, cfg.MonoFSToken, cfg.StoreRoot)
		analyzers := map[string]incident.Analyzer{}
		if cfg.IncidentAgentURL != "" {
			analyzers[incident.AnalysisAgent] = incident.AgentAnalyzer{
				URL:   cfg.IncidentAgentURL,
				Token: cfg.IncidentAgentToken,
			}
		}
		incidentService := &incident.Service{
			Store:         incidentStore,
			Alerts:        store,
			AlertService:  service,
			Engines:       incident.NewEngineRegistry(),
			Analyzers:     analyzers,
			Logger:        log.Default(),
			BaseURL:       cfg.IncidentBaseURL,
			Delivery:      cfg.DeliveryEnabled,
			AsyncAnalysis: true,
		}
		incidentHandler = (&incident.API{
			Store:              incidentStore,
			Service:            incidentService,
			AllowAll:           cfg.AuthAllowAll,
			IdentityHMACSecret: cfg.IdentityHMACSecret,
			ServiceToken:       cfg.ServiceToken,
			Logger:             log.Default(),
		}).Handler()
		api.AckIncident = incidentService.AckByID
		go func() {
			interval := cfg.IncidentTick
			if interval <= 0 {
				interval = 15 * time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			reconcile := func() {
				if err := incidentService.ReconcileTenant(ctx, cfg.DefaultTenant); err != nil && ctx.Err() == nil {
					log.Printf("sentinel: incident reconcile: %v", err)
				}
			}
			reconcile()
			for {
				select {
				case <-ctx.Done():
					return
				case <-alertWake:
					reconcile()
				case <-ticker.C:
					reconcile()
				}
			}
		}()
		log.Printf("sentinel incidents enabled (tick=%s, agent=%t)", cfg.IncidentTick, cfg.IncidentAgentURL != "")
	}

	if cfg.AWSEnabled && cfg.AWSURL != "" {
		source := &alerting.AWSSource{
			BaseURL:     cfg.AWSURL,
			Token:       cfg.AWSToken,
			Regions:     cfg.AWSRegions,
			Tenant:      cfg.DefaultTenant,
			Service:     service,
			Store:       store,
			Logger:      log.Default(),
			Interval:    cfg.AWSPollInterval,
			DefaultMode: alerting.AWSPolicyMode(cfg.AWSAlarmSelection),
		}
		go source.Run(ctx)
		log.Printf("sentinel aws source enabled: %s", cfg.AWSURL)
	}

	if cfg.GCPEnabled && cfg.GCPURL != "" {
		source := &alerting.GCPSource{
			BaseURL:     cfg.GCPURL,
			Token:       cfg.GCPToken,
			Project:     cfg.GCPProject,
			Region:      cfg.GCPRegion,
			Partition:   cfg.GCPPartition,
			Tenant:      cfg.DefaultTenant,
			Service:     service,
			Store:       store,
			Logger:      log.Default(),
			Interval:    cfg.GCPPollInterval,
			DefaultMode: alerting.GCPPolicyMode(cfg.GCPPolicy),
		}
		go source.Run(ctx)
		log.Printf("sentinel gcp source enabled: %s", cfg.GCPURL)
	}

	if cfg.GuardianEnabled && cfg.GuardianURL != "" {
		source := &alerting.GuardianSource{
			BaseURL:  cfg.GuardianURL,
			UIURL:    cfg.GuardianUIURL,
			Token:    cfg.GuardianToken,
			Tenant:   cfg.DefaultTenant,
			Service:  service,
			Store:    store,
			Logger:   log.Default(),
			Interval: cfg.GuardianPollInterval,
		}
		go source.Run(ctx)
		log.Printf("sentinel guardian source enabled: %s", cfg.GuardianURL)
	}

	rootHandler := http.NewServeMux()
	if incidentHandler != nil {
		rootHandler.Handle("/v1/incidents", incidentHandler)
		rootHandler.Handle("/v1/incidents/", incidentHandler)
		rootHandler.Handle("/v1/alert-sets", incidentHandler)
		rootHandler.Handle("/v1/alert-sets/", incidentHandler)
		rootHandler.Handle("/v1/incident-policy", incidentHandler)
		rootHandler.Handle("/v1/postmortem-engines", incidentHandler)
	}
	rootHandler.Handle("/", api.Handler())
	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: auditlog.MutationMiddleware("doctor-sentinel",
			"/v1/alerts", "/v1/alert-sets", "/v1/incidents", "/v1/incident-policy", "/api/v2/")(rootHandler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	tlsCert := strings.TrimSpace(os.Getenv("DOCTOR_SENTINEL_TLS_CERT_FILE"))
	tlsKey := strings.TrimSpace(os.Getenv("DOCTOR_SENTINEL_TLS_KEY_FILE"))
	if (tlsCert != "") != (tlsKey != "") {
		log.Fatal("DOCTOR_SENTINEL_TLS_CERT_FILE and DOCTOR_SENTINEL_TLS_KEY_FILE must both be set")
	}
	serverTLS, err := tlsEnv.ServerTLS(tlsCert, tlsKey)
	if err != nil {
		log.Fatalf("configure HTTP TLS: %v", err)
	}
	server.TLSConfig = serverTLS
	tlsEnabled := serverTLS != nil
	log.Printf("doctor-sentinel listening on %s (store=%s, delivery=%t, tls=%t, tls_mode=%s)", cfg.HTTPAddr, cfg.StoreRoot, cfg.DeliveryEnabled, tlsEnabled, tlsEnv.Mode())
	var serveErr error
	if tlsEnabled {
		serveErr = server.ListenAndServeTLS("", "")
	} else {
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatal(serveErr)
	}
}
