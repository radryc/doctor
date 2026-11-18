package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/radryc/doctor/internal/federation"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
)

func main() {
	var (
		file         = flag.String("file", "", "input catalog snapshot YAML/JSON")
		router       = flag.String("monofs-router", envOr("DOCTOR_MONOFS_ROUTER_ADDR", envOr("DOCTOR_MONOFS_ROUTER", "")), "MonoFS router address (env: DOCTOR_MONOFS_ROUTER_ADDR)")
		token        = flag.String("monofs-token", os.Getenv("DOCTOR_MONOFS_TOKEN"), "MonoFS file-store token (env: DOCTOR_MONOFS_TOKEN; preferred over the flag)")
		root         = flag.String("root", "/doctor/v1/federation", "snapshot root")
		tenant       = flag.String("tenant", "default", "snapshot tenant")
		shardTargets = flag.Int("shard-targets", 500, "targets per shard")
		expected     = flag.String("expected-version", "", "expected current snapshot version (empty = latest/absent)")
		clientID     = flag.String("client-id", "doctor-federation", "MonoFS client id")
		verify       = flag.Bool("verify", false, "read the snapshot back after publishing")
	)
	flag.Parse()

	if strings.TrimSpace(*file) == "" || strings.TrimSpace(*router) == "" || strings.TrimSpace(*token) == "" {
		log.Fatal("--file is required; MonoFS router and token must be supplied via --monofs-router/--monofs-token or DOCTOR_MONOFS_ROUTER_ADDR/DOCTOR_MONOFS_TOKEN")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
		RouterAddr:  *router,
		ClientID:    *clientID,
		Token:       *token,
		PrincipalID: *clientID,
		Role:        "doctor",
		Writable:    true,
	})
	if err != nil {
		log.Fatalf("connect MonoFS file store: %v", err)
	}
	defer client.Close()

	snapshot, err := federation.LoadSnapshotFile(*file)
	if err != nil {
		log.Fatalf("load snapshot: %v", err)
	}

	snapshotStore := federation.NewMonoFSSnapshotStore(client, *token, *root, *tenant)
	snapshotStore.ShardTargets = *shardTargets
	version, err := snapshotStore.Save(ctx, snapshot, *expected)
	if err != nil {
		log.Fatalf("publish snapshot: %v", err)
	}
	log.Printf("published federation catalog version=%s targets=%d shard_targets=%d", version, len(snapshot.Targets), *shardTargets)
	if *verify {
		loaded, err := snapshotStore.Load(ctx)
		if err != nil {
			log.Fatalf("verify snapshot: %v", err)
		}
		log.Printf("verified federation catalog version=%s targets=%d", loaded.Version, len(loaded.Targets))
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
