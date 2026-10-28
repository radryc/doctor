package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"strings"
	"syscall"

	"github.com/radryc/doctor/internal/federation"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
)

func main() {
	var (
		file         = flag.String("file", "", "input catalog snapshot YAML/JSON")
		router       = flag.String("monofs-router", "", "MonoFS router address")
		token        = flag.String("monofs-token", "", "MonoFS file-store token")
		root         = flag.String("root", "/doctor/v1/federation", "snapshot root")
		tenant       = flag.String("tenant", "default", "snapshot tenant")
		shardTargets = flag.Int("shard-targets", 500, "targets per shard")
		expected     = flag.String("expected-version", "", "expected current snapshot version (empty = latest/absent)")
		clientID     = flag.String("client-id", "doctor-federation", "MonoFS client id")
		verify       = flag.Bool("verify", false, "read the snapshot back after publishing")
	)
	flag.Parse()

	if strings.TrimSpace(*file) == "" || strings.TrimSpace(*router) == "" || strings.TrimSpace(*token) == "" {
		log.Fatal("--file, --monofs-router and --monofs-token are required")
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
