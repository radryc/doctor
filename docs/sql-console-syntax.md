# SQL console syntax help

`POST /v1/sql/query` (UI: **SQL console**) runs a single read-only `SELECT` or
`WITH` statement against one of the `logs`, `metrics`, or `traces` views. The
same query surface runs on either backend:

| Backend | When | Semantics |
|---|---|---|
| DuckDB over Parquet | default `--spanengine=duckdb` | each MonoFS node scans its local chunks; results are per-node partials |
| ClickHouse | `--spanengine=clickhouse` | tables are shared; the router runs the statement once |

Both backends use standard SQL string/identifier quoting, so most queries are
portable. Backend-specific functions (JSON, time) are called out below.

## Rules

- One statement only. `SELECT` and `WITH ... SELECT` are allowed; DDL, DML,
  `COPY`, external file functions, and redefining a view are rejected.
- Queries are limited to the selected time range (maximum 7 days), 5,000 rows,
  and 32 KiB of SQL. The console clamps these; the API returns an error when the
  range is invalid.
- `from`/`to` are applied to `timestamp` before your SQL runs, so you do not
  need to repeat the time filter unless you want a narrower slice.
- Read-only: no `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `DROP`, etc.

## Quoting: the most common error

Use **single quotes for text**:

```sql
WHERE service = 'gcp'
```

**Double quotes are identifiers** (column names) in both ClickHouse and
DuckDB. `"gcp"` is parsed as a column reference, which produces:

```
Code: 47. DB::Exception: Unknown expression or function identifier `gcp`
```

Use single quotes, and add wildcards for substring matching:

```sql
-- exact match
WHERE service = 'gcp'

-- contains "gcp" (case-sensitive)
WHERE service LIKE '%gcp%'

-- contains "gcp" (case-insensitive; both engines support ILIKE)
WHERE service ILIKE '%gcp%'
```

`LIKE 'gcp'` without `%` is an exact match, not a search.

## Datasets

### logs

| Column | Type | Notes |
|---|---|---|
| `timestamp` | BIGINT | epoch nanoseconds |
| `time` | TIMESTAMP (DuckDB) / String (ClickHouse) | display timestamp |
| `level` | String | severity text |
| `service` | String | service name |
| `trace_id`, `span_id` | String | correlation IDs |
| `raw_message` | String | log body |
| `labels_json` | String | JSON object of labels |
| `partition` | String | Guardian partition |

### metrics

`time`, `timestamp`, `start_time`, `service`, `metric_name`, `value`,
`metric_type`, `temporality`, `labels_json`, `partition`.

### traces

`time`, `timestamp`, `end_time`, `end_time_ts`, `trace_id`, `span_id`,
`parent_span_id`, `service`, `name`, `status_code`, `attributes_json`,
`partition`.

## Examples

Top services/levels, ordered by row count:

```sql
SELECT service, level, count(*) AS rows
FROM logs
WHERE service ILIKE '%gcp%'
GROUP BY service, level
ORDER BY rows DESC
LIMIT 50
```

Raw matching log rows (the console already applies the time range):

```sql
SELECT time, service, level, raw_message, labels_json
FROM logs
WHERE service ILIKE '%gcp%'
ORDER BY timestamp DESC
LIMIT 50
```

Note: `labels_json AS rows` aliases the JSON text, so `ORDER BY rows` would sort
by that text, not by row count. Use `count(*) AS rows` with `GROUP BY` when you
want counts.

Filter by trace:

```sql
SELECT time, service, name, status_code
FROM traces
WHERE trace_id = '0123456789abcdef0123456789abcdef'
ORDER BY timestamp
```

Error rate by service:

```sql
SELECT service,
       count(*) AS spans,
       countIf(status_code = 'STATUS_CODE_ERROR') AS errors
FROM traces
GROUP BY service
ORDER BY spans DESC
```

Metric average:

```sql
SELECT service, metric_name, avg(value) AS avg_value, max(value) AS max_value
FROM metrics
WHERE metric_name = 'http.server.duration'
GROUP BY service, metric_name
ORDER BY avg_value DESC
```

## Filtering labels

`labels_json` is a JSON **string**, so extract fields before comparing. The
function differs by backend:

```sql
-- DuckDB
WHERE json_extract_string(labels_json, '$.env') = 'prod'

-- ClickHouse
WHERE JSONExtractString(labels_json, 'env') = 'prod'
```

Dots in keys are literal, for example
`json_extract_string(labels_json, '$."http.method"')`.

For traces use `attributes_json` instead of `labels_json`.

## Aggregation and per-node results

With DuckDB/Parquet, chunks are shard-local. The console evaluates the statement
on every healthy node and returns one result per node, so `count`, `sum`, and
`avg` are **partials**, not global totals. The UI shows a tab per node and a
combined row count. Use the Logs, Metrics, or Traces pages when you need a
globally merged answer.

With ClickHouse the tables are shared, so the router executes once and the
result is global.

## Time filters

Prefer the console's range selector. If you need an explicit filter, compare the
numeric nanosecond column:

```sql
WHERE timestamp >= 1762500000000000000
```

`time` is a TIMESTAMP in DuckDB but an ISO-8601 string in ClickHouse, so string
or date functions on `time` are not portable.

## Limits and troubleshooting

| Error | Cause | Fix |
|---|---|---|
| `Unknown expression or function identifier` | text in double quotes | use `'single quotes'` |
| `Referenced column ... not found` | double-quoted string or misspelled column | check the dataset columns above |
| `query time range is limited to 168h0m0s` | range over 7 days | shorten the range |
| `only SELECT statements are allowed` | DDL/DML or multiple statements | run one read-only statement |
| results differ per node | DuckDB chunks are shard-local | aggregate on the dedicated pages, or use ClickHouse |
