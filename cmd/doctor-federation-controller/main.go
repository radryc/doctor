package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/federation"
	monofsstore "github.com/rydzu/ainfra/doctor/internal/store/monofs"
	"github.com/rydzu/ainfra/doctor/internal/telemetry"
)

func main() {
	var (
		guardianURL     = flag.String("guardian-url", envOr("DOCTOR_GUARDIAN_API_URL", ""), "Guardian API base URL")
		guardianToken   = flag.String("guardian-token", envOr("DOCTOR_GUARDIAN_TOKEN", envOr("DOCTOR_ACCESS_POLICY_TOKEN", "")), "Guardian internal machine token")
		monofsRouter    = flag.String("monofs-router", envOr("DOCTOR_MONOFS_ROUTER", ""), "MonoFS router address")
		monofsToken     = flag.String("monofs-token", envOr("DOCTOR_MONOFS_TOKEN", ""), "MonoFS file-store token")
		root            = flag.String("root", "/doctor/v1/federation", "catalog snapshot root")
		tenant          = flag.String("tenant", "default", "federation tenant")
		interval        = flag.Duration("interval", 5*time.Minute, "rebuild interval")
		metadataTTL     = flag.Duration("metadata-ttl", time.Hour, "pusher metadata freshness window")
		metadataTimeout = flag.Duration("metadata-timeout", 5*time.Second, "per-pusher metadata timeout")
		concurrency     = flag.Int("metadata-concurrency", 32, "metadata refresh concurrency")
		shardTargets    = flag.Int("shard-targets", 500, "targets per catalog shard")
		tokenShards     = flag.Int("token-shards", 256, "token bundle shard count")
		leaseTTL        = flag.Duration("lease-ttl", 0, "leader lease TTL (default: 2x interval)")
		holder          = flag.String("holder", defaultHolder(), "leader lease holder identity")
		once            = flag.Bool("once", false, "run one sync and exit")
		dryRun          = flag.Bool("dry-run", false, "build but do not publish")
		clientID        = flag.String("client-id", "doctor-federation-controller", "MonoFS client id")
	)
	flag.Parse()

	if strings.TrimSpace(*guardianURL) == "" || strings.TrimSpace(*monofsRouter) == "" || strings.TrimSpace(*monofsToken) == "" {
		log.Fatal("--guardian-url, --monofs-router and --monofs-token are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	telemetryCfg, err := telemetry.LoadConfig("doctor-federation-controller")
	if err != nil {
		log.Fatalf("load telemetry config: %v", err)
	}
	telemetryHandle, err := telemetry.Setup(ctx, telemetryCfg)
	if err != nil {
		log.Fatalf("set up telemetry: %v", err)
	}
	if telemetryHandle.Enabled() {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = telemetryHandle.Shutdown(shutdownCtx)
		}()
	}

	client, err := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
		RouterAddr:  *monofsRouter,
		ClientID:    *clientID,
		Token:       *monofsToken,
		PrincipalID: *holder,
		Role:        "doctor",
		Writable:    true,
	})
	if err != nil {
		log.Fatalf("connect MonoFS file store: %v", err)
	}
	defer client.Close()

	controller, err := federation.NewController(federation.ControllerConfig{
		GuardianURL:          *guardianURL,
		GuardianToken:        *guardianToken,
		SnapshotRoot:         *root,
		Tenant:               *tenant,
		MonoFSToken:          *monofsToken,
		Interval:             *interval,
		MetadataTTL:          *metadataTTL,
		MetadataTimeout:      *metadataTimeout,
		MetadataConcurrency:  *concurrency,
		SnapshotShardTargets: *shardTargets,
		TokenShardCount:      *tokenShards,
		LeaseTTL:             *leaseTTL,
		Holder:               *holder,
		DryRun:               *dryRun,
	}, client)
	if err != nil {
		log.Fatalf("configure federation controller: %v", err)
	}
	log.Printf("federation controller starting: guardian=%s root=%s tenant=%s interval=%s dry_run=%v once=%v",
		*guardianURL, *root, *tenant, *interval, *dryRun, *once)
	if err := controller.Run(ctx, *once); err != nil {
		log.Fatalf("federation controller: %v", err)
	}
}

func defaultHolder() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "doctor-federation-controller"
	}
	return hostname
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
