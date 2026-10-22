package dashboards

// builtInPresets returns the embedded system preset dashboards.
// These are read-only (BuiltIn = true) but can be cloned into editable shared dashboards.
func builtInPresets() []DashboardDocument {
	return []DashboardDocument{
		presetFleetOverview(),
		presetServiceRED(),
		presetSynthetics(),
		presetMonoFSStorage(),
		presetDoctorSelf(),
		presetAWSFederation(),
	}
}

func presetFleetOverview() DashboardDocument {
	return DashboardDocument{
		Version:          Version,
		ID:               "preset-guardian-fleet",
		Title:            "Strata & Guardian Fleet Overview",
		Description:      "Partition health, reconcile rates, drifted/failing assets, and recent error logs.",
		Tenant:           "*",
		Tags:             []string{"fleet", "guardian", "preset"},
		BuiltIn:          true,
		DefaultTimeRange: "1h",
		RefreshInterval:  "30s",
		Variables: []DashboardVariable{
			{Name: "partition", Label: "Partition", Type: VariablePartition, DefaultValue: "$__all", IncludeAll: true},
		},
		Layout: []DashboardWidget{
			{
				ID: "fleet-partitions-total", Title: "Total Partitions", Kind: WidgetStat,
				GridPos:       GridPos{X: 0, Y: 0, W: 3, H: 3},
				Queries:       []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus, Expr: `count(count by (guardian_partition) (guardian_asset_observation_current{guardian_partition=~"$partition"}))`}},
				Visualization: &WidgetVisualization{Unit: "short"},
			},
			{
				ID: "fleet-healthy-assets", Title: "Healthy Assets", Kind: WidgetStat,
				GridPos:       GridPos{X: 3, Y: 0, W: 3, H: 3},
				Queries:       []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus, Expr: `sum(guardian_asset_observation_current{status="Healthy",guardian_partition=~"$partition"})`}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "red"}, {Value: 1, Color: "green"}}},
			},
			{
				ID: "fleet-failing-assets", Title: "Failing Assets", Kind: WidgetStat,
				GridPos:       GridPos{X: 6, Y: 0, W: 3, H: 3},
				Queries:       []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus, Expr: `sum(guardian_asset_observation_current{status=~"Unhealthy|Failed|Critical|Attention",guardian_partition=~"$partition"}) or vector(0)`}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 1, Color: "amber"}, {Value: 5, Color: "red"}}},
			},
			{
				ID: "fleet-drifted-intents", Title: "Drifted Intents", Kind: WidgetStat,
				GridPos:       GridPos{X: 9, Y: 0, W: 3, H: 3},
				Queries:       []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus, Expr: `sum(guardian_dispatcher_intent_status_current{status=~"Drifted.*"}) or vector(0)`}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 1, Color: "amber"}}},
			},
			{
				ID: "fleet-reconcile-rate", Title: "Reconcile Executions/s", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr:         `sum(rate(guardian_reconciler_partition_executions[$__rate_interval])) by (guardian_partition)`,
					LegendFormat: "{{guardian_partition}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "reqps"},
			},
			{
				ID: "fleet-dispatcher-writes", Title: "Dispatcher Writes/s", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 6, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr:         `sum(rate(guardian_dispatcher_writes_total[$__rate_interval]))`,
					LegendFormat: "dispatcher",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "reqps"},
			},
			{
				ID: "fleet-error-logs", Title: "Recent Error Logs", Kind: WidgetLogs,
				GridPos: GridPos{X: 0, Y: 9, W: 12, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceLoki,
					Expr: `{guardian_partition=~"$partition"} |= "error"`,
				}},
			},
		},
	}
}

func presetServiceRED() DashboardDocument {
	return DashboardDocument{
		Version:          Version,
		ID:               "preset-service-red",
		Title:            "Service RED Overview (APM Template)",
		Description:      "Request Rate, Error Ratio, and Latency percentiles with Exemplars for any service.",
		Tenant:           "*",
		Tags:             []string{"apm", "red", "service", "preset"},
		BuiltIn:          true,
		DefaultTimeRange: "1h",
		RefreshInterval:  "30s",
		Variables: []DashboardVariable{
			{Name: "partition", Label: "Partition", Type: VariablePartition, DefaultValue: "$__all", IncludeAll: true},
			{Name: "service", Label: "Service", Type: VariableService, DefaultValue: "$__all", LabelKey: "service", IncludeAll: true},
		},
		Layout: []DashboardWidget{
			{
				ID: "red-rps", Title: "Request Rate (RPS)", Kind: WidgetStat,
				GridPos: GridPos{X: 0, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(rate(http_server_request_duration_count{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) or sum(rate(http_requests_total{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval]))`,
				}},
				Visualization: &WidgetVisualization{Unit: "reqps"},
			},
			{
				ID: "red-error-ratio", Title: "Error Ratio", Kind: WidgetStat,
				GridPos: GridPos{X: 3, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `(sum(rate(http_server_request_duration_count{service=~"$service",guardian_partition=~"$partition",http_response_status_code=~"5.."}[$__rate_interval])) or sum(rate(http_requests_total{service=~"$service",guardian_partition=~"$partition",status_code=~"5.."}[$__rate_interval]))) / (sum(rate(http_server_request_duration_count{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) or sum(rate(http_requests_total{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval]))) * 100`,
				}},
				Visualization: &WidgetVisualization{Unit: "percent", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 1, Color: "amber"}, {Value: 5, Color: "red"}}},
			},
			{
				ID: "red-p95", Title: "P95 Latency", Kind: WidgetStat,
				GridPos: GridPos{X: 6, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `histogram_quantile(0.95, sum(rate(http_server_request_duration_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000 or histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000`,
				}},
				Visualization: &WidgetVisualization{Unit: "ms", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 200, Color: "amber"}, {Value: 1000, Color: "red"}}},
			},
			{
				ID: "red-active-series", Title: "Active Series", Kind: WidgetStat,
				GridPos: GridPos{X: 9, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `count({__name__=~".+",service=~"$service(\\..*)?")}`,
				}},
				Visualization: &WidgetVisualization{Unit: "short"},
			},
			{
				ID: "red-latency-chart", Title: "Request Latency (p50/p95/p99) + Exemplars", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{
					{RefID: "A", Datasource: DatasourcePrometheus, LegendFormat: "p50",
						Expr: `histogram_quantile(0.50, sum(rate(http_server_request_duration_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000 or histogram_quantile(0.50, sum(rate(http_request_duration_seconds_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000`},
					{RefID: "B", Datasource: DatasourcePrometheus, LegendFormat: "p95",
						Expr: `histogram_quantile(0.95, sum(rate(http_server_request_duration_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000 or histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000`},
					{RefID: "C", Datasource: DatasourcePrometheus, LegendFormat: "p99",
						Expr: `histogram_quantile(0.99, sum(rate(http_server_request_duration_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000 or histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{service=~"$service",guardian_partition=~"$partition"}[$__rate_interval])) by (le)) * 1000`},
				},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "ms", ShowExemplars: true},
			},
			{
				ID: "red-top-failing-ops", Title: "Top Failing Operations", Kind: WidgetToplist,
				GridPos: GridPos{X: 6, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceTempo,
					Expr:         `{resource.service.name=~"$service" && status = error} | rate() by (span.name)`,
					LegendFormat: "{{span.name}}",
				}},
				Visualization: &WidgetVisualization{Unit: "reqps"},
			},
			{
				ID: "red-error-logs", Title: "Recent Error Logs", Kind: WidgetLogs,
				GridPos: GridPos{X: 0, Y: 9, W: 6, H: 5},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceLoki,
					Expr: `{service=~"$service",guardian_partition=~"$partition"} |= "error"`,
				}},
			},
			{
				ID: "red-slow-traces", Title: "Recent Slow Traces (> 200ms)", Kind: WidgetTraces,
				GridPos: GridPos{X: 6, Y: 9, W: 6, H: 5},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceTempo,
					Expr: `{resource.service.name=~"$service" && duration > 200ms}`,
				}},
			},
		},
	}
}

func presetSynthetics() DashboardDocument {
	return DashboardDocument{
		Version:          Version,
		ID:               "preset-synthetics",
		Title:            "Synthetic Probes & SLO",
		Description:      "Guardian blackbox probe availability, latency, failure streaks, and correlated logs/traces.",
		Tenant:           "*",
		Tags:             []string{"synthetics", "probes", "guardian", "preset"},
		BuiltIn:          true,
		DefaultTimeRange: "1h",
		RefreshInterval:  "30s",
		Variables: []DashboardVariable{
			{Name: "partition", Label: "Partition", Type: VariablePartition, DefaultValue: "$__all", IncludeAll: true},
			{Name: "service", Label: "Service", Type: VariableService, DefaultValue: "$__all", LabelKey: "service", IncludeAll: true},
		},
		Layout: []DashboardWidget{
			{
				ID: "synthetics-up", Title: "Probes Up", Kind: WidgetStat,
				GridPos: GridPos{X: 0, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(guardian_probe_up{service=~"$service",guardian_partition=~"$partition"})`,
				}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "red"}, {Value: 1, Color: "green"}}},
			},
			{
				ID: "synthetics-failing", Title: "Failing Probes", Kind: WidgetStat,
				GridPos: GridPos{X: 3, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(guardian_probe_up{service=~"$service",guardian_partition=~"$partition"} == 0) or vector(0)`,
				}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 1, Color: "amber"}, {Value: 5, Color: "red"}}},
			},
			{
				ID: "synthetics-avg-latency", Title: "Avg Probe Latency", Kind: WidgetStat,
				GridPos: GridPos{X: 6, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `avg(guardian_probe_duration_seconds{service=~"$service",guardian_partition=~"$partition"}) * 1000`,
				}},
				Visualization: &WidgetVisualization{Unit: "ms", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 500, Color: "amber"}, {Value: 2000, Color: "red"}}},
			},
			{
				ID: "synthetics-fail-streak", Title: "Max Consecutive Failures", Kind: WidgetStat,
				GridPos: GridPos{X: 9, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `max(guardian_probe_consecutive_failures{service=~"$service",guardian_partition=~"$partition"}) or vector(0)`,
				}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 1, Color: "amber"}, {Value: 5, Color: "red"}}},
			},
			{
				ID: "synthetics-up-chart", Title: "Probe Availability by Probe", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr:         `min by (guardian_probe_name) (guardian_probe_up{service=~"$service",guardian_partition=~"$partition"})`,
					LegendFormat: "{{guardian_probe_name}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "short"},
			},
			{
				ID: "synthetics-latency-chart", Title: "Probe Latency by Partition", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 6, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr:         `avg by (guardian_partition) (guardian_probe_duration_seconds{service=~"$service",guardian_partition=~"$partition"}) * 1000`,
					LegendFormat: "{{guardian_partition}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "ms"},
			},
			{
				ID: "synthetics-probe-logs", Title: "Probe Transition Logs", Kind: WidgetLogs,
				GridPos: GridPos{X: 0, Y: 9, W: 6, H: 5},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceLoki,
					Expr: `{guardian_partition=~"$partition", guardian_probe_name=~".+"}`,
				}},
			},
			{
				ID: "synthetics-probe-traces", Title: "Probe Failure Traces", Kind: WidgetTraces,
				GridPos: GridPos{X: 6, Y: 9, W: 6, H: 5},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceTempo,
					Expr: `{name=~"guardian.probe.*" && status = error}`,
				}},
			},
		},
	}
}

func presetMonoFSStorage() DashboardDocument {
	return DashboardDocument{
		Version:          Version,
		ID:               "preset-monofs-storage",
		Title:            "MonoFS & LogEngine Storage",
		Description:      "MonoFS operations, read/write throughput, query duration, and KVS active keys.",
		Tenant:           "*",
		Tags:             []string{"monofs", "storage", "logengine", "preset"},
		BuiltIn:          true,
		DefaultTimeRange: "1h",
		RefreshInterval:  "30s",
		Layout: []DashboardWidget{
			{
				ID: "monofs-ops", Title: "MonoFS Ops/s", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 0, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(rate(monofs_server_ops_total[$__rate_interval])) by (op)`, LegendFormat: "{{op}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "reqps"},
			},
			{
				ID: "monofs-throughput", Title: "Read/Write Throughput", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 6, Y: 0, W: 6, H: 6},
				Queries: []WidgetQuery{
					{RefID: "A", Datasource: DatasourcePrometheus, LegendFormat: "read",
						Expr: `sum(rate(monofs_server_read_bytes_total[$__rate_interval]))`},
					{RefID: "B", Datasource: DatasourcePrometheus, LegendFormat: "write",
						Expr: `sum(rate(monofs_server_ingest_bytes_total[$__rate_interval]))`},
				},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "bps"},
			},
			{
				ID: "monofs-query-p95", Title: "MonoFS Query Duration p95", Kind: WidgetStat,
				GridPos: GridPos{X: 0, Y: 6, W: 4, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `histogram_quantile(0.95, sum(rate(monofs_logengine_query_duration_seconds_bucket[$__rate_interval])) by (le)) * 1000`,
				}},
				Visualization: &WidgetVisualization{Unit: "ms"},
			},
			{
				ID: "monofs-kvs-keys", Title: "KVS Active Keys", Kind: WidgetStat,
				GridPos: GridPos{X: 4, Y: 6, W: 4, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(kvs_active_keys)`,
				}},
				Visualization: &WidgetVisualization{Unit: "short"},
			},
			{
				ID: "monofs-pebble-commits", Title: "Pebble Batch Commits/s", Kind: WidgetStat,
				GridPos: GridPos{X: 8, Y: 6, W: 4, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(rate(kvs_pebble_batch_commits_total[$__rate_interval]))`,
				}},
				Visualization: &WidgetVisualization{Unit: "reqps"},
			},
			{
				ID: "monofs-pruning", Title: "LogEngine Chunk Pruning Efficiency", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 9, W: 12, H: 6},
				Queries: []WidgetQuery{
					{RefID: "A", Datasource: DatasourcePrometheus, LegendFormat: "listed {{signal}}",
						Expr: `sum(rate(monofs_logengine_query_chunks_listed_total[$__rate_interval])) by (signal)`},
					{RefID: "B", Datasource: DatasourcePrometheus, LegendFormat: "pruned {{signal}}",
						Expr: `sum(rate(monofs_logengine_query_chunks_pruned_total[$__rate_interval])) by (signal)`},
				},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "short"},
			},
		},
	}
}

func presetDoctorSelf() DashboardDocument {
	return DashboardDocument{
		Version:          Version,
		ID:               "preset-doctor-self",
		Title:            "Doctor Self-Observability",
		Description:      "Component heartbeats, ingest requests, buffer backlog, query latency, and proxy endpoint status.",
		Tenant:           "*",
		Tags:             []string{"doctor", "self", "preset"},
		BuiltIn:          true,
		DefaultTimeRange: "1h",
		RefreshInterval:  "30s",
		Layout: []DashboardWidget{
			{
				ID: "doctor-up", Title: "Components Up", Kind: WidgetStat,
				GridPos: GridPos{X: 0, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(doctor_component_up)`,
				}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "red"}, {Value: 1, Color: "green"}}},
			},
			{
				ID: "doctor-ingest-rps", Title: "Ingest Accepted/s", Kind: WidgetStat,
				GridPos: GridPos{X: 3, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(rate(doctor_ingest_requests_accepted[$__rate_interval]))`,
				}},
				Visualization: &WidgetVisualization{Unit: "reqps"},
			},
			{
				ID: "doctor-queue-depth", Title: "Trace Buffer Pending Records", Kind: WidgetStat,
				GridPos: GridPos{X: 6, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `sum(doctor_ingest_buffer_pending_records{signal="traces"})`,
				}},
				Visualization: &WidgetVisualization{Unit: "short", Thresholds: []ThresholdLevel{{Value: 0, Color: "green"}, {Value: 1000, Color: "amber"}, {Value: 5000, Color: "red"}}},
			},
			{
				ID: "doctor-eval-latency", Title: "Query HTTP Latency p95", Kind: WidgetStat,
				GridPos: GridPos{X: 9, Y: 0, W: 3, H: 3},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr: `histogram_quantile(0.95, sum(rate(http_server_request_duration_bucket{guardian_component="doctor-query"}[$__rate_interval])) by (le)) * 1000`,
				}},
				Visualization: &WidgetVisualization{Unit: "ms"},
			},
			{
				ID: "doctor-ingest-chart", Title: "Ingest Throughput by Signal", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{
					{RefID: "A", Datasource: DatasourcePrometheus, LegendFormat: "{{signal}}",
						Expr: `sum by (signal) (rate(doctor_ingest_requests_accepted[$__rate_interval]))`},
				},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "reqps"},
			},
			{
				ID: "doctor-proxy-endpoints", Title: "Federated Proxy Endpoint Status", Kind: WidgetTable,
				GridPos: GridPos{X: 6, Y: 3, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceNativeTraces,
					Expr: `/v1/proxy/endpoints`,
				}},
			},
		},
	}
}

func presetAWSFederation() DashboardDocument {
	return DashboardDocument{
		Version:          Version,
		ID:               "preset-aws-federation",
		Title:            "AWS CloudWatch & X-Ray Federation",
		Description:      "Pre-scoped to source=aws: federated EC2/Lambda/RDS metrics, CloudWatch log volume, and X-Ray trace summaries.",
		Tenant:           "*",
		Tags:             []string{"aws", "cloudwatch", "xray", "preset"},
		BuiltIn:          true,
		DefaultTimeRange: "1h",
		RefreshInterval:  "60s",
		Variables: []DashboardVariable{
			{Name: "region", Label: "AWS Region", Type: VariableCustom,
				Options:      []string{"us-east-1", "us-west-2", "eu-west-1", "eu-central-1", "ap-southeast-1"},
				DefaultValue: "$__all", IncludeAll: true},
			{Name: "service", Label: "AWS Service", Type: VariableService, DefaultValue: "$__all", IncludeAll: true},
		},
		Layout: []DashboardWidget{
			{
				ID: "aws-ec2-cpu", Title: "EC2 CPU Utilization", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 0, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr:         `aws_ec2_cpuutilization_average{region=~"$region"}`,
					LegendFormat: "{{instance_id}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "percent"},
			},
			{
				ID: "aws-lambda-duration", Title: "Lambda Duration p95", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 6, Y: 0, W: 6, H: 6},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourcePrometheus,
					Expr:         `aws_lambda_duration_p95{region=~"$region"}`,
					LegendFormat: "{{function_name}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "line", Unit: "ms"},
			},
			{
				ID: "aws-cw-log-volume", Title: "CloudWatch Log Volume", Kind: WidgetTimeseries,
				GridPos: GridPos{X: 0, Y: 6, W: 6, H: 5},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceLoki,
					Expr:         `sum(count_over_time({source="aws",aws_region=~"$region"}[$__rate_interval])) by (aws_log_group)`,
					LegendFormat: "{{aws_log_group}}",
				}},
				Visualization: &WidgetVisualization{DisplayMode: "area", Stacked: true, Unit: "short"},
			},
			{
				ID: "aws-xray-traces", Title: "X-Ray Trace Summaries", Kind: WidgetTraces,
				GridPos: GridPos{X: 6, Y: 6, W: 6, H: 5},
				Queries: []WidgetQuery{{RefID: "A", Datasource: DatasourceTempo,
					Expr: `{ source = "aws" && region =~ "$region" }`,
				}},
			},
		},
	}
}
