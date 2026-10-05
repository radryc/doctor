package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func benchmarkVector(source string, seriesCount int) map[string]any {
	result := make([]any, 0, seriesCount)
	for i := 0; i < seriesCount; i++ {
		result = append(result, map[string]any{
			"metric": map[string]any{
				"__name__":  "http_requests_total",
				"instance":  fmt.Sprintf("instance-%d", i),
				"job":       "api",
				LabelSource: source,
			},
			"value": []any{float64(1780000000 + i), "1"},
		})
	}
	return map[string]any{"resultType": "vector", "result": result}
}

func BenchmarkMergePrometheusSeriesOverlap(b *testing.B) {
	const backendCount = 16
	const seriesCount = 10_000

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		remotes := make([]PromRemote, 0, backendCount)
		for j := 0; j < backendCount; j++ {
			backend := fmt.Sprintf("prom-%d", j)
			remotes = append(remotes, PromRemote{
				Source:  "prometheus",
				Backend: backend,
				Data:    benchmarkVector("prometheus", seriesCount),
			})
		}
		local := benchmarkVector(SourceDoctor, seriesCount)
		b.StartTimer()
		_ = MergePrometheusQueryData(local, "vector", remotes...)
	}
}

func BenchmarkMergeLogsNoOverlap(b *testing.B) {
	const recordCount = 10_000
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	local := make([]segment.LogRecord, 0, recordCount)
	remote := make([]segment.LogRecord, 0, recordCount)
	for i := 0; i < recordCount; i++ {
		local = append(local, logRecord(base.Add(time.Duration(i)*time.Millisecond), SourceDoctor, fmt.Sprintf("local-%d", i)))
		remote = append(remote, logRecord(base.Add(time.Duration(i)*time.Millisecond), SourceAWS, fmt.Sprintf("remote-%d", i)))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MergeLogs(local, LogRemote{Source: SourceAWS, Records: remote})
	}
}
