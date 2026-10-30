package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/radryc/doctor/internal/alerting"
	"github.com/radryc/doctor/internal/config"
	"github.com/radryc/doctor/internal/ruler"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.LoadSentinel("doctor-sentinel")
	if !cfg.Enabled {
		log.Fatal("doctor-sentinel is disabled (DOCTOR_SENTINEL_ENABLED=false)")
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
	service := &alerting.Service{
		Store:           store,
		DefaultTenant:   cfg.DefaultTenant,
		GuardianHMACKey: []byte(cfg.GuardianHMACSecret),
		DeliveryEnabled: cfg.DeliveryEnabled,
		Logger:          log.Default(),
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
		Store:        store,
		Service:      service,
		Logger:       log.Default(),
		AllowAll:     cfg.AuthAllowAll,
		ServiceToken: cfg.ServiceToken,
	}

	dispatcher := &alerting.Dispatcher{Store: store, Logger: log.Default()}
	if cfg.DeliveryEnabled {
		go dispatcher.Run(ctx)
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

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: api.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("doctor-sentinel listening on %s (store=%s, delivery=%t)", cfg.HTTPAddr, cfg.StoreRoot, cfg.DeliveryEnabled)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
