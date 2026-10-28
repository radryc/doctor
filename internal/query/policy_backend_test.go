package query

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

// rawOnlyBackend embeds LogEngineBackend so it can be passed to policyBackend;
// only QueryLogsRaw is exercised by this test.
type rawOnlyBackend struct {
	LogEngineBackend
	gotQuery string
	records  []segment.LogRecord
}

func (b *rawOnlyBackend) QueryLogsRaw(_ context.Context, _ string, query string, _, _ time.Time, _ int) ([]segment.LogRecord, error) {
	b.gotQuery = query
	return b.records, nil
}

func TestPolicyBackendQueryLogsRawDelegates(t *testing.T) {
	raw := &rawOnlyBackend{records: []segment.LogRecord{{Body: "hello"}}}
	backend := &policyBackend{inner: raw}
	query := `{service=~".+"} |= "hello"`

	records, err := backend.QueryLogsRaw(context.Background(), "default", query, time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatalf("QueryLogsRaw: %v", err)
	}
	if raw.gotQuery != query {
		t.Fatalf("inner query = %q, want %q", raw.gotQuery, query)
	}
	if len(records) != 1 || records[0].Body != "hello" {
		t.Fatalf("records = %#v", records)
	}
}
