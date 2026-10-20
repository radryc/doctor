package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/config"
	"github.com/rydzu/ainfra/doctor/internal/ruler"
	monofsstore "github.com/rydzu/ainfra/doctor/internal/store/monofs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.LoadRules("doctor-rules")
	if !cfg.Enabled {
		log.Fatal("doctor-rules is disabled (DOCTOR_RULES_ENABLED=false)")
	}
	if cfg.MonoFSLogEngineAddr == "" {
		log.Fatal("DOCTOR_MONOFS_LOGENGINE_ADDR is required")
	}
	if cfg.MonoFSToken == "" {
		log.Fatal("DOCTOR_MONOFS_TOKEN is required to read and write rules in MonoFS")
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
	api := &ruler.API{Store: store, Evaluator: evaluator, Scheduler: scheduler, Lease: lease, Logger: log.Default()}
	go lease.Run(ctx, scheduler.Run)

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: api.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("doctor-rules listening on %s (ruler=%s, store=%s)", cfg.HTTPAddr, cfg.RulerID, cfg.StoreRoot)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
