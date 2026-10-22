# ClickHouse via Guardian Partition Assets — Production Design (S3 Backend)

- **Status:** Draft
- **Date:** 2026-10-22
- **Scope:** Guardian deploys ClickHouse; MonoFS uses it as the trace store;
  traces live in S3 with a small local metadata/cache volume.
- **Related:** [ClickHouse traces plan](./clickhouse-traces-plan.md),
  [Multi-account federation design](./multi-account-federation-50k-design.md)

---

## 1. Decision

Use **generic Guardian assets** for ClickHouse:

- `Volume` — `/var/lib/clickhouse` (metadata, S3 disk metadata, cache; small)
- `Config` — `config.d/guardian.xml` (S3 disk + storage policy)
- `Secret` — ClickHouse password and S3 credentials
- `Compute` — `clickhouse/clickhouse-server`
- Compute service ports — `9000` native, `8123` HTTP, `9363` Prometheus

**MonoFS is the only ClickHouse client.** Doctor does not get a ClickHouse DSN;
it keeps talking to MonoFS. Guardian only runs the container.

Do **not** extend Guardian’s `Database` asset for ClickHouse in v1. The
`Database` asset currently supports PostgreSQL/MySQL/MariaDB and owns
engine-specific behavior. ClickHouse stays a generic Compute asset, so Guardian
does not grow a ClickHouse driver.

Do **not** build ClickHouse cluster orchestration, replication, backups, or
schema migration into Guardian. Guardian manages the Kubernetes object
lifecycle; MonoFS owns the schema and query semantics.

---

## 2. Production topology

Small initial production:

```text
                         S3: strata-clickhouse-prod/clickhouse/
                                      ▲
                                      │ S3 disk (data parts)
                                      │
                         ┌────────────┴────────────┐
                         │ ClickHouse (1 node)      │
                         │ 30 GiB PVC: metadata +   │
                         │ 20 GiB local cache       │
                         └────────────▲────────────┘
                                      │ native :9000
                         ┌────────────┴────────────┐
                         │ MonoFS server nodes      │
                         │ logs/metrics -> logengine│
                         │ traces -> ClickHouse     │
                         └────────────▲────────────┘
                                      │ gRPC
                         ┌────────────┴────────────┐
                         │ MonoFS router            │
                         │ trace reads -> one node  │
                         └────────────▲────────────┘
                                      │
                         Doctor ingest/query / Grafana
```

- ClickHouse is internal-only; no public LoadBalancer.
- S3 is the data tier; the PVC is for metadata/cache, not the full trace data.
- MonoFS router reads traces from **one** MonoFS node because ClickHouse is
  shared. Fanning out to all nodes would duplicate results.
- MonoFS writes are still hashed across nodes; all nodes use the same ClickHouse
  DSN.
- Logs/metrics remain in MonoFS.

Scaling path:

1. Increase ClickHouse CPU/memory/cache.
2. Add ClickHouse read replicas or move to ClickHouse Cloud.
3. Add/remove MonoFS nodes; trace nodes are stateless with respect to ClickHouse.
4. For fully stateless logs/metrics, add a real S3 backend to the MonoFS
   logengine (separate workstream).

A single self-hosted ClickHouse node is not HA. If trace queries are
business-critical, use ClickHouse Cloud or a 3-node cluster from the start.

---

## 3. Partition layout

```text
stratatools/partitions/clickhouse/
  config.yaml
  intents/
    clickhouse.yaml
  payloads/
    clickhouse.k8s.yaml      # optional service annotations / serviceAccountName
```

Partition config:

```yaml
apiVersion: guardian/v1alpha1
kind: Partition
metadata:
  name: clickhouse
  labels:
    role: observability
    component: clickhouse
    managedBy: guardian
spec:
  defaults:
    probes:
      enabled: true
      interval: 30s
      timeout: 5s
      retries: 3
      httpPaths: ["/ping"]
      tcpPorts: [9000, 8123]
  deletionPolicy: orphan
  reconciliation:
    mode: auto
    interval: 1m
  labels:
    stack: k8s
    cloud: aws
    data: traces
```

---

## 4. Intent example (S3)

Replace bucket, region, and endpoint with the real values. Pin a current
ClickHouse LTS image; `25.8` below is an example, not a hard requirement.

```yaml
apiVersion: guardian/v1alpha1
kind: Intent
metadata:
  name: clickhouse
spec:
  intentType: standard
  targetPusher: k8s-main
  target:
    cluster: k8s-main
    namespace: clickhouse
  locked: false
  assets:
    - type: Volume
      name: clickhouse-meta
      properties:
        size: 30Gi
        accessMode: ReadWriteOnce

    - type: Config
      name: clickhouse-config
      properties:
        format: xml
        data:
          guardian.xml: |
            <clickhouse>
              <listen_host>0.0.0.0</listen_host>
              <max_server_memory_usage>6442450944</max_server_memory_usage>
              <mark_cache_size>1073741824</mark_cache_size>
              <uncompressed_cache_size>0</uncompressed_cache_size>

              <logger>
                <level>information</level>
                <console>1</console>
              </logger>

              <prometheus>
                <endpoint>/metrics</endpoint>
                <port>9363</port>
              </prometheus>

              <storage_configuration>
                <disks>
                  <s3>
                    <type>s3</type>
                    <endpoint>https://s3.us-east-1.amazonaws.com/strata-clickhouse-prod/clickhouse/</endpoint>
                    <region>us-east-1</region>
                    <use_environment_credentials>true</use_environment_credentials>
                    <metadata_path>/var/lib/clickhouse/disks/s3/</metadata_path>
                  </s3>
                  <s3_cache>
                    <type>cache</type>
                    <disk>s3</disk>
                    <path>/var/lib/clickhouse/disks/s3_cache/</path>
                    <max_size>20Gi</max_size>
                  </s3_cache>
                </disks>
                <policies>
                  <s3_main>
                    <volumes>
                      <main>
                        <disk>s3_cache</disk>
                      </main>
                    </volumes>
                  </s3_main>
                </policies>
              </storage_configuration>
            </clickhouse>

    - type: Secret
      name: clickhouse-auth
      properties:
        value: "change-me-clickhouse-password"

    - type: Secret
      name: s3-access-key
      properties:
        value: "change-me-s3-access-key"

    - type: Secret
      name: s3-secret-key
      properties:
        value: "change-me-s3-secret-key"

    - type: Compute
      name: clickhouse
      dependsOn:
        - clickhouse-meta
        - clickhouse-config
        - clickhouse-auth
        - s3-access-key
        - s3-secret-key
      properties:
        image: clickhouse/clickhouse-server:25.8
        imagePullPolicy: IfNotPresent
        ports:
          - name: native
            containerPort: 9000
            servicePort: 9000
            protocol: TCP
          - name: http
            containerPort: 8123
            servicePort: 8123
            protocol: TCP
          - name: metrics
            containerPort: 9363
            servicePort: 9363
            protocol: TCP
        env:
          CLICKHOUSE_DB: strata
          CLICKHOUSE_USER: strata
          CLICKHOUSE_PASSWORD: ${intent.secrets.outputs.clickhouse-auth.value}
          CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: "1"
          AWS_ACCESS_KEY_ID: ${intent.secrets.outputs.s3-access-key.value}
          AWS_SECRET_ACCESS_KEY: ${intent.secrets.outputs.s3-secret-key.value}
          AWS_REGION: us-east-1
        volumeMounts:
          - volume: clickhouse-meta
            path: /var/lib/clickhouse
        configMounts:
          - config: clickhouse-config
            path: /etc/clickhouse-server/config.d
            readOnly: true
        resources:
          limits:
            cpu: "4"
            memory: 8Gi
          requests:
            cpu: "2"
            memory: 4Gi
        healthCheck:
          test:
            - CMD-SHELL
            - >-
              clickhouse-client --user "$CLICKHOUSE_USER"
              --password "$CLICKHOUSE_PASSWORD" --query "SELECT 1" >/dev/null 2>&1
          interval: 30s
          timeout: 5s
          retries: 3
```

Notes:

- The Compute `ports` create the internal service. The DNS name is normally
  `clickhouse.clickhouse.svc.cluster.local`; verify with
  `kubectl -n clickhouse get svc`.
- No public LoadBalancer.
- The `Secret` values above are placeholders. For production, use `secretRef`
  or an external secret manager; do not commit real credentials.
- The `Config` mount is a `config.d` merge, so the image's default config stays
  intact.
- `use_environment_credentials` lets ClickHouse read the AWS env vars from the
  Secret. For an instance role, omit the access-key envs and ensure the node
  role has the S3 policy.
- For MinIO, use the MinIO endpoint (bucket root) and MinIO access/secret keys.
  The cache and storage policy are the same.
- The healthcheck uses `clickhouse-client` inside the image and the same envs.
- `s3_cache` max size must fit inside the 30 GiB PVC along with metadata. Keep
  headroom; 20 GiB cache is a starting point.

---

## 5. ClickHouse table

MonoFS creates the schema at startup (idempotent):

```sql
CREATE DATABASE IF NOT EXISTS strata;

CREATE TABLE IF NOT EXISTS strata.spans
(
    tenant          LowCardinality(String),
    timestamp       DateTime64(9, 'UTC'),
    end_time        DateTime64(9, 'UTC'),
    duration_ns     UInt64,
    trace_id        String,
    span_id         String,
    parent_span_id  String,
    service         LowCardinality(String),
    name            String,
    status_code     LowCardinality(String),
    attributes      Map(String, String),
    INDEX trace_id_idx trace_id TYPE bloom_filter GRANULARITY 4,
    INDEX service_idx service TYPE set(1000) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant, service, toStartOfHour(timestamp), trace_id, span_id)
TTL toDateTime(timestamp) + INTERVAL 30 DAY DELETE
SETTINGS
    index_granularity = 8192,
    ttl_only_drop_parts = 1,
    storage_policy = 's3_main';
```

- One table; no materialized views or projections in v1.
- Daily partitions make TTL deletes predictable.
- `trace_id` is high-cardinality; bloom filter only.
- Direct trace lookups must be time-bounded (default 7 days when absent).
- Add a `trace_index` table only if p95 trace lookup needs it.
- Schema changes are additive (`ALTER TABLE ... ADD COLUMN`); track a
  `schema_migrations` table in MonoFS.

---

## 6. MonoFS wiring

ClickHouse is used only by `monofs-server`; the router only selects a reader
node.

### 6.1 Monofs server

```yaml
env:
  MONOFS_TRACE_BACKEND: clickhouse
  MONOFS_CLICKHOUSE_DSN: clickhouse://strata:<password>@clickhouse.clickhouse.svc.cluster.local:9000/strata
  MONOFS_CLICKHOUSE_TABLE: strata.spans
  MONOFS_CLICKHOUSE_TTL_DAYS: "30"
  MONOFS_CLICKHOUSE_BATCH_SIZE: "2000"
  MONOFS_CLICKHOUSE_FLUSH_INTERVAL: "2s"
```

- `monofs-server` still needs `--logengine-dir=/data/logengine` for logs/metrics.
- The hybrid backend delegates logs/metrics to the logengine and traces to
  ClickHouse.
- The DSN password must come from a Secret in the MonoFS partition (or a shared
  secret namespace). Do not put it in a ConfigMap.
- If ClickHouse is down, traces fail; logs/metrics stay available.

### 6.2 Monofs router

```yaml
env:
  MONOFS_TRACE_BACKEND: clickhouse
```

- Trace reads go to **one** healthy MonoFS node; fail over on error.
- Trace writes stay hashed across nodes; all nodes write to the same ClickHouse
  table.
- Without this, router fanout returns duplicate spans from every node.

### 6.3 Tenant

Add `tenant` to `SpanEntry` and `QueryTracesRequest`; Doctor sets it from its
tenant context. MonoFS writes and filters the `tenant` column. Do not run
multi-tenant traces without this.

---

## 7. S3 bucket and IAM

Bucket:

```text
s3://strata-clickhouse-prod/clickhouse/
```

Recommended:

- Block all public access.
- SSE-S3 or SSE-KMS.
- Versioning on.
- Lifecycle: abort incomplete multipart uploads after 7 days; expire
  noncurrent versions after 30 days.
- Do not add a lifecycle rule that deletes current objects; ClickHouse TTL owns
  retention.

IAM policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:ListBucket",
        "s3:GetBucketLocation",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": [
        "arn:aws:s3:::strata-clickhouse-prod",
        "arn:aws:s3:::strata-clickhouse-prod/*"
      ]
    }
  ]
}
```

Use a dedicated role/user for ClickHouse. Do not reuse the MonoFS bucket
credentials.

### 7.1 Who creates the bucket

Guardian's Kubernetes `ObjectStore` asset deploys MinIO; it is not an AWS S3
bucket provisioner. For production S3, use one of:

- pre-create the bucket with Terraform/CLI and put the endpoint in the
  ClickHouse `Config`;
- use a Guardian AWS `ObjectStore` asset in an AWS-targeted intent and reference
  its bucket/region outputs.

Do not use the Kubernetes MinIO `ObjectStore` asset for production S3.

For EKS, an instance role is the simplest path. If using IRSA, verify that the
ClickHouse version's AWS SDK picks up the web-identity token; otherwise use the
Secret env vars above. Do not assume IRSA works without a smoke test.

---

## 8. Volume permissions

The official ClickHouse image runs as the `clickhouse` user (UID 101). A fresh
PVC may be root-owned, causing startup failure when ClickHouse writes
`/var/lib/clickhouse`.

Guardian `ComputeSpec` does not expose `securityContext`/`fsGroup` or
`initContainers` in v1. Options:

1. Pre-create the volume with the correct ownership / use a storage class that
   supports `fsGroup`.
2. Use a wrapper entrypoint:
   `chown -R clickhouse:clickhouse /var/lib/clickhouse && exec /entrypoint.sh`.
   Validate the image entrypoint path; it is version-sensitive.
3. Extend `ComputeSpec` with `securityContext`/`fsGroup` if this repeats. That
   is a Guardian core change, not part of this design.

This is the most likely first failure in kind. Check it before debugging S3.

---

## 9. Backup and recovery

S3 is the data tier, not a complete backup:

- ClickHouse metadata and S3 disk metadata live on the PVC.
- Back up the PVC, or use `clickhouse-backup` to S3 on a schedule.
- Managed ClickHouse provides backups; use it if we do not want to own this.
- Test a restore of `strata.spans` before calling the deployment production.

Do not claim that S3 versioning alone is a ClickHouse backup.

---

## 10. Operations

Health:

```bash
kubectl -n clickhouse get pods,svc,pvc
kubectl -n clickhouse exec deploy/clickhouse -- \
  clickhouse-client --query "SELECT version()"
kubectl -n clickhouse exec deploy/clickhouse -- \
  clickhouse-client --query "SELECT count() FROM strata.spans"
```

Disk/parts:

```sql
SELECT table, partition, sum(bytes_on_disk) AS bytes
FROM system.parts
WHERE active AND database = 'strata' AND table = 'spans'
GROUP BY table, partition
ORDER BY partition DESC;

SELECT * FROM system.mutations WHERE is_done = 0;
```

S3/cache:

```sql
SELECT * FROM system.disks;
SELECT * FROM system.storage_policies;
```

Upgrade:

1. Pin the new ClickHouse LTS image in the partition.
2. Release the partition (`gc release run --partition clickhouse --bump`).
3. Expect a short single-node outage; drain trace queries or rely on Doctor's
   retry/WAL.
4. Run the schema check and a trace smoke test.
5. ClickHouse upgrades are forward-only; do not downgrade in place.

---

## 11. What Guardian manages vs what it does not

| Concern | Guardian | MonoFS | Notes |
| --- | --- | --- | --- |
| ClickHouse container lifecycle | ✅ | — | Compute asset |
| Local volume | ✅ | — | Volume asset |
| Config/secret mounts | ✅ | — | Config/Secret assets |
| Health/probes | ✅ | — | `/ping`, TCP 9000/8123 |
| Service DNS | ✅ | — | Compute ports |
| S3 bucket/IAM | external/IaC | — | Guardian asset optional |
| Schema creation/migration | — | ✅ | `CREATE ... IF NOT EXISTS` |
| Retention/TTL | — | ✅ | Table TTL |
| Query semantics | — | ✅ | TraceStore |
| Router trace read selection | — | ✅ | One node when shared |
| Backups/restore | — | — | Managed or external tooling |
| Replication/HA/Keeper | — | — | Managed or dedicated operator |
| ClickHouse upgrades | image bump | schema check | Single-node downtime |

---

## 12. Effort and risks

### Effort

| Work | Estimate |
| --- | --- |
| ClickHouse partition + S3 config + demo bring-up | 3–4 days |
| Volume permission / image validation | 0.5–1 day |
| MonoFS ClickHouse TraceStore + hybrid backend | 4–6 days |
| Router single-read + tenant protocol | 3–4 days |
| S3/IAM/runbook/backup test | 2–3 days |
| **Total minimal production** | **~3–4 weeks** |

Managed ClickHouse removes most of the operational work; the MonoFS integration
effort is unchanged.

### Risks

| Risk | Mitigation |
| --- | --- |
| PVC ownership / CrashLoop | Pre-create volume, wrapper, or extend ComputeSpec |
| Single-node outage | Managed ClickHouse or 3-node cluster for HA |
| ClickHouse OOM | Memory limits, `max_server_memory_usage`, batching |
| Too many parts | Batch inserts, daily partitions, TTL, monitor `system.parts` |
| S3 credential/IRSA issue | Static Secret envs; smoke test IRSA first |
| Local metadata loss | Back up PVC / metadata; managed service |
| Duplicate trace results | Router single-node read when ClickHouse is shared |
| Tenant leakage | Add `tenant` to proto and every query |
| Guardian scope creep | Generic Compute only; no ClickHouse operator |

---

## 13. Bottom line

- **Guardian deploys one small ClickHouse node** with a small metadata/cache PVC.
- **S3 stores the trace data**; the node can be replaced without moving data.
- **MonoFS uses ClickHouse for traces** through its existing backend interface.
- **Doctor and Grafana do not change** except for tenant propagation.
- **One table, TTL, batch inserts, one reader node, feature flag.**
- **Scale by adding ClickHouse compute/replicas or moving to managed
  ClickHouse**; do not rewrite the platform.

This is the smallest production-shaped deployment that can grow without making
ClickHouse a second platform to operate.
