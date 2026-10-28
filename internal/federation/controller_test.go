package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/ruler"
)

type fakeGuardianSource struct {
	pushers    []PusherInfo
	tokens     map[string]string
	partitions []PartitionInfo
	err        error
}

func (f *fakeGuardianSource) Pushers(context.Context) ([]PusherInfo, map[string]string, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.pushers, f.tokens, nil
}

func (f *fakeGuardianSource) Partitions(context.Context) ([]PartitionInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.partitions, nil
}

type fakeMetadataSource struct {
	meta TargetMeta
	err  error
}

func (f *fakeMetadataSource) Fetch(context.Context, string, string) (TargetMeta, error) {
	if f.err != nil {
		return TargetMeta{}, f.err
	}
	return f.meta, nil
}

func newControllerForTest(t *testing.T) (*Controller, *ruler.MemoryStore, *fakeGuardianSource) {
	t.Helper()
	files := ruler.NewMemoryStore()
	controller, err := NewController(ControllerConfig{
		GuardianURL:     "http://guardian",
		SnapshotRoot:    "/doctor/v1/federation",
		Tenant:          "default",
		MetadataTTL:     time.Hour,
		MetadataTimeout: time.Second,
		Interval:        time.Minute,
		Holder:          "test-controller",
	}, files)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	source := &fakeGuardianSource{
		pushers: []PusherInfo{
			{Name: "aws-a", Account: "111111111111", Region: "us-east-1", Endpoint: "http://pusher-a", TokenRef: "guardian-pusher://aws-a", Version: "v1"},
			{Name: "aws-b", Account: "222222222222", Region: "us-east-1", Endpoint: "http://pusher-b", TokenRef: "guardian-pusher://aws-b", Version: "v1"},
		},
		tokens: map[string]string{"aws-a": "token-a", "aws-b": "token-b"},
		partitions: []PartitionInfo{
			{Name: "div-a", Account: "111111111111", Region: "us-east-1", Provider: "aws"},
			{Name: "div-b", Account: "222222222222", Region: "us-east-1", Provider: "aws"},
		},
	}
	controller.Guardian = source
	controller.Metadata = &fakeMetadataSource{meta: TargetMeta{MetricNames: []string{"Invocations"}, LogGroups: []string{"guardian/111/us-east-1/div-a/app"}}}
	return controller, files, source
}

func TestControllerSyncPublishesCatalogTokensAndMetadata(t *testing.T) {
	controller, files, _ := newControllerForTest(t)
	if err := controller.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	snapshot, err := controller.Store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load snapshot: %v", err)
	}
	if len(snapshot.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(snapshot.Targets))
	}
	if snapshot.Targets[0].TokenRef != "guardian-pusher://aws-a" {
		t.Fatalf("token ref = %q", snapshot.Targets[0].TokenRef)
	}
	if len(snapshot.Targets[0].MetricNames) == 0 {
		t.Fatalf("metadata was not attached: %#v", snapshot.Targets[0])
	}

	resolver := NewBundleTokenResolver(BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: controller.Config.TokenShardCount,
	})
	for _, target := range snapshot.Targets {
		token, err := resolver.Resolve(context.Background(), target)
		if err != nil {
			t.Fatalf("resolve %s: %v", target.ID, err)
		}
		if token == "" {
			t.Fatalf("empty token for %s", target.ID)
		}
	}
	cachePath := controller.metadataPath()
	if raw, err := files.ReadFile(context.Background(), cachePath); err != nil || len(raw) == 0 {
		t.Fatalf("metadata cache %s not saved: %v", cachePath, err)
	}

	firstVersion := snapshot.Version
	if err := controller.Sync(context.Background()); err != nil {
		t.Fatalf("unchanged Sync: %v", err)
	}
	unchanged, err := controller.Store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load unchanged snapshot: %v", err)
	}
	if unchanged.Version != firstVersion {
		t.Fatalf("unchanged catalog was republished: %s -> %s", firstVersion, unchanged.Version)
	}
}

func TestControllerSyncKeepsLastKnownMetadataOnFailure(t *testing.T) {
	controller, _, source := newControllerForTest(t)
	if err := controller.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	// New pusher version forces a refresh; the fetch fails but the prior
	// metadata must remain in the next catalog.
	for index := range source.pushers {
		source.pushers[index].Version = "v2"
	}
	controller.Metadata = &fakeMetadataSource{err: errors.New("pusher unavailable")}
	if err := controller.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	snapshot, err := controller.Store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load snapshot: %v", err)
	}
	if len(snapshot.Targets) != 2 || len(snapshot.Targets[0].MetricNames) == 0 {
		t.Fatalf("last-known metadata lost: %#v", snapshot.Targets)
	}
}

func TestLeaderLeaseElectsOneHolderAndExpires(t *testing.T) {
	files := ruler.NewMemoryStore()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	first := NewLeaderLease(files, "", "/doctor/v1/federation", "default", "first", 30*time.Second)
	second := NewLeaderLease(files, "", "/doctor/v1/federation", "default", "second", 30*time.Second)
	first.Now = func() time.Time { return now }
	second.Now = func() time.Time { return now }

	acquired, err := first.Acquire(context.Background())
	if err != nil || !acquired {
		t.Fatalf("first Acquire = %v, %v", acquired, err)
	}
	acquired, err = second.Acquire(context.Background())
	if err != nil || acquired {
		t.Fatalf("second Acquire = %v, %v, want false", acquired, err)
	}
	now = now.Add(31 * time.Second)
	acquired, err = second.Acquire(context.Background())
	if err != nil || !acquired {
		t.Fatalf("second Acquire after expiry = %v, %v", acquired, err)
	}
	renewed, err := first.Renew(context.Background())
	if err != nil || renewed {
		t.Fatalf("first Renew after takeover = %v, %v", renewed, err)
	}
}

func TestTokenBundleWriterOnlyRewritesChangedShards(t *testing.T) {
	files := ruler.NewMemoryStore()
	writer := NewTokenBundleWriter(files, "", "/doctor/v1/federation/tokens", "default", 4)
	tokens := map[string]string{"aws-a": "a", "aws-b": "b"}
	changed, err := writer.Write(context.Background(), tokens)
	if err != nil || changed == 0 {
		t.Fatalf("first Write = %d, %v", changed, err)
	}
	changed, err = writer.Write(context.Background(), tokens)
	if err != nil || changed != 0 {
		t.Fatalf("second Write = %d, %v", changed, err)
	}
	tokens["aws-c"] = "c"
	changed, err = writer.Write(context.Background(), tokens)
	if err != nil || changed == 0 || changed > 1 {
		t.Fatalf("rotated Write = %d, %v", changed, err)
	}
}

func TestGuardianFeedPaginatesPushersAndPartitions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Guardian-Internal-Token"); got != "internal" {
			t.Errorf("internal token = %q", got)
		}
		switch r.URL.Path {
		case "/api/m2m/pushers":
			page := guardianPusherPage{Total: 2}
			if r.URL.Query().Get("cursor") == "" {
				page.Pushers = []guardianPusher{{Name: "aws-a", Account: "111", Region: "us-east-1", ObservedAPIURL: "http://a", Token: "token-a"}}
				page.NextCursor = "aws-a"
				page.HasMore = true
			} else {
				page.Pushers = []guardianPusher{{Name: "aws-b", Account: "222", Region: "us-east-1", ObservedAPIURL: "http://b", Token: "token-b"}}
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/api/m2m/federation/partitions":
			page := guardianPartitionPage{Total: 2}
			if r.URL.Query().Get("cursor") == "" {
				page.Partitions = []guardianPartition{{Name: "div-a", Account: "111", Region: "us-east-1", Provider: "aws"}}
				page.NextCursor = "div-a"
				page.HasMore = true
			} else {
				page.Partitions = []guardianPartition{{Name: "div-b", Account: "222", Region: "us-east-1", Provider: "aws"}}
			}
			_ = json.NewEncoder(w).Encode(page)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	feed := NewGuardianFeed(server.URL, "internal", server.Client())
	pushers, tokens, err := feed.Pushers(context.Background())
	if err != nil {
		t.Fatalf("Pushers: %v", err)
	}
	if len(pushers) != 2 || tokens["aws-a"] != "token-a" || tokens["aws-b"] != "token-b" {
		t.Fatalf("pushers = %#v tokens = %#v", pushers, tokens)
	}
	if pushers[0].TokenRef != "guardian-pusher://aws-a" {
		t.Fatalf("token ref = %q", pushers[0].TokenRef)
	}
	partitions, err := feed.Partitions(context.Background())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if len(partitions) != 2 || partitions[1].Name != "div-b" {
		t.Fatalf("partitions = %#v", partitions)
	}
}
