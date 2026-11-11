# Partition quotas

Partition quotas limit how much one partition, user, group, or client can consume. The quota key is
the Guardian partition; `tenant` is the cluster/routing label and is not a quota dimension.

## Sources

- **Partition envelope:** `Partition.spec.quota` in the Guardian partition manifest.
- **Profiles, defaults, and subject quotas:** `quota_profiles`, `quota_defaults`, and `quotas` in the
  repo-managed `access.yaml`.
- **Distribution:** Guardian compiles both into the existing policy bundle at
  `/api/access/policy`. The bundle keeps `format_version: 1`; the new `quotas` field is optional so
  older consumers ignore it during a rolling upgrade.

## Resolution

1. Start from `quota_defaults` in `access.yaml`, then environment defaults.
2. Apply the partition's `profile`.
3. Apply the partition's explicit overrides.
4. Charge the request against the partition bucket, the user bucket, every matching group bucket,
   and the client bucket. A request passes only when every applicable bucket has capacity.

`0` means "not configured" (inherit/unlimited). Use `blocked: true` for an emergency stop.

## Built-in defaults

When `access.yaml` has no `quota_defaults`, every partition receives these built-in values (override
with `quota_defaults` or `DOCTOR_QUOTA_DEFAULT_*`):

| Resource | Default |
|---|---|
| traces/logs/metrics ingest | 10 MiB/s, 20k records/s, 4 MiB batch |
| metric samples | 50k/s, 250k active series, 5k metric names, 20k series/metric, 64 labels |
| query | 50 req/s, 10 concurrent, 1000 budget/s, 168h range, 2M samples, 50k series, 8 partitions |
| rules | 50 groups, 500 rules, 100 rules/group, 500 evaluation cost/s |
| storage | 500 GiB, traces 7d, logs 14d, metrics 30d |

## Enforcement points

| Limit | Enforced by |
|---|---|
| ingest bytes/s, records/s, max batch | `doctor-ingest` |
| metric names, active series, series per metric, labels | `doctor-ingest` |
| query requests/s, concurrency, cost budget | `doctor-query` middleware |
| query max range, max estimated samples, max partitions | `doctor-query` middleware |
| query max result series | `doctor-query` Prometheus/Loki/Tempo result guards |
| recording rule groups and rules | `doctor-rules` API |
| rule evaluation cost/s | `doctor-rules` scheduler token budget |
| storage bytes over the retention window | `doctor-ingest` rolling-window admission (approximate) |

Unknown partitions are rejected in enforce mode. In observe mode they are charged to the `unknown`
bucket and logged.

## Modes

- `observe` (default): violations are logged and counted but the request succeeds.
- `enforce`: violations return HTTP `429` / gRPC `ResourceExhausted`.

`DOCTOR_QUOTA_ENFORCE=true` sets the deployment default. A partition or profile may override it.

## Example partition

```yaml
spec:
  quota:
    enforcement: observe
    ingest:
      traces: { bytesPerSecond: 5242880, recordsPerSecond: 20000, maxBatchBytes: 4194304 }
      logs: { bytesPerSecond: 5242880, recordsPerSecond: 20000 }
      metrics:
        bytesPerSecond: 5242880
        samplesPerSecond: 50000
        maxActiveSeries: 100000
    query: { requestsPerSecond: 50, maxConcurrent: 10, maxRange: 168h }
    rules: { maxGroups: 25, maxRules: 500, maxRulesPerGroup: 100 }
```

## Example access policy

```yaml
quota_profiles:
  standard: { query: { requestsPerSecond: 50, maxConcurrent: 10 } }
  premium:  { query: { requestsPerSecond: 200, maxConcurrent: 40 } }

quota_defaults:
  query: { requestsPerSecond: 20, maxConcurrent: 4 }

quotas:
  groups:
    platform: { profile: premium }
  users:
    alice@example.com: { query: { requestsPerSecond: 100, maxConcurrent: 10 } }
  clients:
    grafana: { query: { requestsPerSecond: 200, maxConcurrent: 20 } }
```

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `DOCTOR_QUOTA_ENABLED` | true when a policy URL is set | enable quota loading/enforcement |
| `DOCTOR_QUOTA_POLICY_URL` | `DOCTOR_ACCESS_POLICY_URL` | compiled policy bundle URL |
| `DOCTOR_QUOTA_POLICY_TOKEN` | `DOCTOR_ACCESS_POLICY_TOKEN` | bearer token for the bundle |
| `DOCTOR_QUOTA_POLICY_INTERVAL` | `30s` | refresh interval |
| `DOCTOR_QUOTA_REPLICAS` | `1` | divide every rate across replicas |
| `DOCTOR_QUOTA_ENFORCE` | `false` | deployment default mode |
| `DOCTOR_QUOTA_DEFAULT_QUERY_RPS` | built-in | fallback query requests/s per partition |
| `DOCTOR_QUOTA_DEFAULT_QUERY_CONCURRENT` | built-in | fallback query concurrency |
| `DOCTOR_QUOTA_DEFAULT_QUERY_BUDGET` | built-in | fallback query cost budget/s |
| `DOCTOR_QUOTA_DEFAULT_QUERY_MAX_RANGE` | built-in | fallback maximum query range |
| `DOCTOR_QUOTA_DEFAULT_QUERY_MAX_SAMPLES` | built-in | fallback estimated sample cap |
| `DOCTOR_QUOTA_DEFAULT_QUERY_MAX_SERIES` | built-in | fallback result series cap |
| `DOCTOR_QUOTA_DEFAULT_INGEST_BYTES_PER_SECOND` | built-in | fallback ingest bytes/s |
| `DOCTOR_QUOTA_DEFAULT_INGEST_RECORDS_PER_SECOND` | built-in | fallback ingest records/s |
| `DOCTOR_QUOTA_DEFAULT_MAX_SERIES` | built-in | fallback active-series cap |
| `DOCTOR_QUOTA_DEFAULT_RULES_MAX` | built-in | fallback rules per partition |
| `DOCTOR_QUOTA_DEFAULT_RULES_COST` | built-in | fallback rule evaluation cost/s |
| `DOCTOR_QUOTA_DEFAULT_STORAGE_BYTES` | built-in | fallback storage byte cap |
| `DOCTOR_INGEST_URL` | empty | doctor-query proxy target for ingest usage |
| `DOCTOR_INGEST_AUTH_TOKEN` | empty | token for the ingest usage endpoint |

`DOCTOR_QUOTA_REPLICAS` is a safe upper bound only when traffic is spread across replicas. If gRPC
clients pin to one replica, configure client-side round-robin or move to a lease-based coordinator.

## Dashboard

`doctor-query` serves `GET /v1/quota` and the UI has a **Quotas** page. It shows:

- the caller's user/group/client buckets;
- each visible partition's effective limits and mode;
- current query usage from the query engine;
- optional ingest usage proxied from `doctor-ingest` at `GET /v1/quota/usage`.

Usage bars are per replica because enforcement is local; the configured limits on the left are the
aggregate envelope. Set `DOCTOR_QUOTA_REPLICAS` to the actual replica count so the per-replica bucket
matches `limit / replicas`.
