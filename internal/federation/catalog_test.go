package federation

import (
	"testing"
)

func TestLoadCatalogResolvesTokenEnvAndIndexes(t *testing.T) {
	t.Setenv("TEST_PUSHER_TOKEN_A", "token-a")
	t.Setenv("TEST_PUSHER_TOKEN_B", "token-b")

	raw := []byte(`
targets:
  - id: aws-a
    account: "111111111111"
    region: us-east-1
    partitions: [div-a]
    endpoint: http://pusher-a:19090
    token_env: TEST_PUSHER_TOKEN_A
    metric_names: [Invocations]
    log_groups: [guardian/111111111111/us-east-1/div-a/app]
    xray_services: [svc-a]
  - id: aws-b
    account: "222222222222"
    region: us-east-1
    partitions: [div-b]
    endpoint: http://pusher-b:19090
    token_env: TEST_PUSHER_TOKEN_B
`)
	catalog, err := LoadCatalog(raw)
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	if catalog.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", catalog.Len())
	}
	if got := catalog.ByAccounts([]string{"111111111111"}); len(got) != 1 || got[0].Token != "token-a" {
		t.Fatalf("ByAccounts() = %#v", got)
	}
	if got := catalog.ByPartitions([]string{"div-b"}); len(got) != 1 || got[0].ID != "aws-b" {
		t.Fatalf("ByPartitions() = %#v", got)
	}
	if got := catalog.ByLogGroups([]string{"guardian/111111111111/us-east-1/div-a/app"}); len(got) != 1 || got[0].ID != "aws-a" {
		t.Fatalf("ByLogGroups() = %#v", got)
	}
	if got := catalog.ByServices([]string{"svc-a"}); len(got) != 1 || got[0].ID != "aws-a" {
		t.Fatalf("ByServices() = %#v", got)
	}
	if got := catalog.ByMetric("Invocations"); len(got) != 1 || got[0].ID != "aws-a" {
		t.Fatalf("ByMetric() = %#v", got)
	}
}

func TestLoadCatalogRejectsMissingTokenEnv(t *testing.T) {
	t.Setenv("TEST_PUSHER_TOKEN_MISSING", "")
	_, err := LoadCatalog([]byte(`
targets:
  - id: aws-a
    account: "111111111111"
    region: us-east-1
    endpoint: http://pusher-a:19090
    token_env: TEST_PUSHER_TOKEN_MISSING
`))
	if err == nil {
		t.Fatal("expected missing token_env error")
	}
}
