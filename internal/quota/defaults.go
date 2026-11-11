package quota

import "github.com/radryc/monofs/pkg/authz/policy"

// Defaults returns the built-in envelope applied to every partition when the
// policy bundle does not define quota_defaults. They are intentionally
// generous enough for normal workloads but bound every noisy-neighbour class.
func Defaults() policy.Quota {
	const (
		ingestBytes   = 10 << 20 // 10 MiB/s
		ingestRecords = 20000
		maxBatch      = 4 << 20 // 4 MiB
	)
	return policy.Quota{
		Ingest: &policy.IngestQuota{
			Traces: &policy.SignalIngestQuota{
				BytesPerSecond:   ingestBytes,
				RecordsPerSecond: ingestRecords,
				MaxBatchBytes:    maxBatch,
			},
			Logs: &policy.SignalIngestQuota{
				BytesPerSecond:   ingestBytes,
				RecordsPerSecond: ingestRecords,
				MaxBatchBytes:    maxBatch,
			},
			Metrics: &policy.MetricsIngestQuota{
				BytesPerSecond:     ingestBytes,
				RecordsPerSecond:   ingestRecords,
				MaxBatchBytes:      maxBatch,
				SamplesPerSecond:   50000,
				MaxMetricNames:     5000,
				MaxActiveSeries:    250000,
				MaxSeriesPerMetric: 20000,
				MaxLabelsPerSeries: 64,
			},
		},
		Query: &policy.QueryQuota{
			RequestsPerSecond: 50,
			MaxConcurrent:     10,
			BudgetPerSecond:   1000,
			MaxRange:          "168h",
			MaxSamples:        2000000,
			MaxSeries:         50000,
			MaxPartitions:     8,
		},
		Rules: &policy.RulesQuota{
			MaxGroups:                  50,
			MaxRules:                   500,
			MaxRulesPerGroup:           100,
			MaxEvaluationCostPerSecond: 500,
		},
		Storage: &policy.StorageQuota{
			MaxBytes:         500 << 30, // 500 GiB
			RetentionTraces:  "168h",
			RetentionLogs:    "336h",
			RetentionMetrics: "720h",
		},
	}
}
