# Cleat Data Services: Bundled Value on PostgreSQL

## Strategy

"Cleat gives you what DBOS does, *plus* scalable data management on the same
database you're already running."

DBOS sells durable execution on Postgres. Cleat can sell durable execution
PLUS blob storage, event sourcing, and job queues — all backed by the same
PostgreSQL instance. The pitch: your database investment does more.

## What DBOS Requires

DBOS needs PostgreSQL. That's it. You get durable execution.

## What Cleat Could Require

Cleat also needs PostgreSQL. But with the same database, you ALSO get:

1. **Blob Store** — S3 for bytes, PostgreSQL for metadata. Query blobs by tags,
   hash, timestamp. Content-addressable dedup. Signed URLs for direct upload.
2. **Event Store** — `event_history` generalized beyond workflow events.
   Append-only, queryable by time range, tenant-scoped. Natural fit for
   event sourcing, audit logs, activity feeds.
3. **Job Queue** — `workflow_instances` with `SKIP LOCKED` is already a
   reliable queue. Expose it as a standalone API for non-workflow use.
4. **KV Store** — `query_state` JSONB with key-level access. Configuration,
   feature flags, session state.

The architecture is already built. `workflow_instances` IS a job queue.
`event_history` IS an event store. `query_state` IS a KV store. Adding blob
storage makes the platform useful for applications that don't even need
durable execution — but once they're using cleat for blob storage, adopting
workflows is a natural next step.

---

## 1. Blob Store

### Problem
S3 is cheap but dumb — you can't query blobs by metadata. Every team builds
the same thing: an S3 bucket + a PostgreSQL table with `path, size, content_type,
hash, tags, created_at`. Cleat can provide this out of the box.

### Design

```
┌──────────────────────────────────────────┐
│  cleat blob store API                    │
│  PUT /blobs/:key    (multipart upload)   │
│  GET /blobs/:key    (redirect to S3)     │
│  DELETE /blobs/:key                      │
│  GET /blobs?tag=foo&prefix=bar&limit=50  │
│  GET /blobs/:key/metadata                │
└──────────┬───────────────────────────────┘
           │
    ┌──────▼──────┐     ┌──────────────┐
    │  PostgreSQL  │     │  S3 / GCS     │
    │  (metadata)  │     │  (blob bytes) │
    │              │     │               │
    │  blob_index  │     │  bucket/cleat │
    │  - key       │     │  /<hash>      │
    │  - size      │     │               │
    │  - sha256    │     │               │
    │  - content_  │     │               │
    │    type      │     │               │
    │  - tags      │     │               │
    │  - tenant_id │     │               │
    │  - expires_at│     │               │
    └─────────────┘     └──────────────┘
```

### Schema

```sql
CREATE TABLE blob_index (
    key         TEXT NOT NULL,
    tenant_id   UUID NOT NULL,
    sha256      BYTEA NOT NULL,        -- content hash for dedup
    size        BIGINT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    tags        JSONB NOT NULL DEFAULT '{}',
    expires_at  TIMESTAMPTZ,           -- optional TTL
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);

-- Content-addressed storage: many keys can point to the same blob
CREATE TABLE blob_content (
    sha256      BYTEA PRIMARY KEY,
    s3_url      TEXT NOT NULL,         -- s3://bucket/cleat/<sha256_hex>
    size        BIGINT NOT NULL,
    ref_count   INTEGER NOT NULL DEFAULT 1,  -- number of keys referencing this content
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Index for metadata queries
CREATE INDEX idx_blob_tags ON blob_index USING gin(tags);
CREATE INDEX idx_blob_tenant_created ON blob_index(tenant_id, created_at DESC);
CREATE INDEX idx_blob_expires ON blob_index(expires_at) WHERE expires_at IS NOT NULL;
```

### Key Behaviors

**Content-addressed dedup**: When the same file is uploaded under different
keys (or by different tenants with shared blobs), only one copy is stored in
S3. `blob_content.ref_count` tracks references. This is automatic and
transparent — the user doesn't need to know about it.

**Signed URLs**: `GET /blobs/:key` returns a 302 redirect to a pre-signed S3
URL (valid for 5 minutes). The blob bytes never pass through the cleat worker.
For upload, `PUT /blobs/:key` returns a pre-signed S3 upload URL. The client
uploads directly to S3, then calls `POST /blobs/:key/commit` with the resulting
S3 key to finalize the metadata.

**Tiered TTLs**: `expires_at` triggers automatic deletion. Deleted blobs
decrement `ref_count`. When `ref_count` hits 0, the S3 object is deleted.
This enables: temporary file uploads, log rotation, cache entries.

**Size limits**: Configurable per tenant. Default 100 MB per blob, 10 GB total
per tenant. Large blobs use S3 multipart upload with presigned URLs for each
part.

### Use Cases

- **Workflow inputs/outputs**: Workflows produce reports, PDFs, images. Store
  them in the blob store, return the key in the workflow result.
- **Document management**: Upload contracts, invoices, receipts. Query by
  `tags.customer_id`, `tags.invoice_date`.
- **ML model registry**: Store model weights. Tags: `framework=pytorch`,
  `version=1.2.3`, `accuracy=0.94`. Content-addressing ensures dedup.
- **Build artifacts**: CI/CD pipelines store binaries. Query by
  `tags.commit_sha`, `tags.branch`.
- **User uploads**: Profile pictures, attachments. TTLs for temporary files.

### Why This Over Raw S3

| Raw S3 | Cleat Blob Store |
|--------|------------------|
| List objects by prefix only | Query by tags, date ranges, content type |
| No built-in dedup | Content-addressed, automatic dedup |
| No TTL (needs lifecycle rules) | Per-blob `expires_at` |
| No multi-tenancy | `tenant_id` isolation + RLS |
| API is S3-specific | REST API, consistent with workflow API |
| Access control via IAM | Access control via API keys |

---

## 2. Event Store

### Problem
Many applications need an append-only event log: audit trails, activity feeds,
change data capture, event sourcing. Building one on PostgreSQL is well-
understood but tedious. Cleat already has `event_history` — generalize it.

### Design

```sql
CREATE TABLE event_stream (
    stream_id   TEXT NOT NULL,         -- logical stream (e.g., "user:123")
    sequence    INTEGER NOT NULL,      -- monotonic within stream
    tenant_id   UUID NOT NULL,
    event_type  TEXT NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, stream_id, sequence)
);
```

### API

```
POST /events/:stream    -- append an event, returns {sequence: 42}
GET /events/:stream     -- read events, ?from=0&limit=100
GET /events/:stream/live -- SSE stream of new events
```

### Why This Over Manual

Same PostgreSQL. Same API key auth. Same tenant isolation. No new
infrastructure. Events from workflows and events from application code
live in the same system, queryable together.

---

## 3. Job Queue

### Problem
`workflow_instances` with `SKIP LOCKED` is already a reliable job queue.
Expose it directly for non-workflow background jobs.

### Design

No new schema — reuse `workflow_instances`. Add a lightweight mode where
the workflow body is a no-op and the "result" is the job output.

```
POST /jobs/:queue    -- enqueue, returns {job_id}
GET /jobs/:queue/:id -- check status/result
```

Workers can claim from `task_queue = 'jobs'` to process these alongside
workflows, or dedicated workers can use `--task-queue jobs` to process
only background jobs.

### Why This Over Sidekiq/Bull/Etc

Same PostgreSQL. No Redis. No separate infrastructure. Jobs and workflows
share the same scheduling, retry, and observability infrastructure. One
`/metrics` endpoint for everything.

---

## 4. Key-Value Store

### Problem
Workflows already use `query_state` (JSONB key-value) for progress reporting.
Generalize it for application configuration, feature flags, session state.

### Design

```sql
CREATE TABLE kv_store (
    key         TEXT NOT NULL,
    tenant_id   UUID NOT NULL,
    value       JSONB NOT NULL DEFAULT 'null',
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);
```

### API

```
GET /kv/:key           -- {value, version, updated_at}
PUT /kv/:key           -- conditional on version for optimistic concurrency
GET /kv?prefix=config/ -- list keys by prefix
```

---

## 5. Positioning vs DBOS

### The Comparison Table

| Capability | DBOS | Cleat |
|-----------|------|-------|
| Durable execution on Postgres | Yes | Yes |
| Workflow versioning | Application-level decorators | WASM blobs in DB, deploy via INSERT |
| Worker model | Embedded in app | Stateless, independently scalable |
| Blob store with metadata queries | No | Yes (S3 + PG index, content-addressed) |
| Event store | No | Yes (append-only, queryable, SSE) |
| Job queue | Via workflows | Yes (standalone + workflow-integrated) |
| KV store | No | Yes (versioned, optimistic concurrency) |
| Multi-tenancy | Via namespaces | tenant_id on every row, API key auth, RLS |
| Helm chart + Grafana | No | Yes |
| OpenTelemetry tracing | Yes | Yes |
| Languages | TS, Python, Go, Java | Go, Rust, Java/Kotlin, AssemblyScript |

### The Pitch

"DBOS gives you durable execution. Cleat gives you durable execution PLUS
blob storage, event sourcing, a job queue, and a KV store — all on the same
PostgreSQL database. Your infrastructure investment does more."

### The Bundle Logic

Every cleat user already has PostgreSQL. Adding blob storage, event streams,
and job queues doesn't add infrastructure cost — it adds value to
infrastructure they already pay for. The marginal cost of these features
is nearly zero (a few more tables, a few hundred lines of Go). The marginal
value is significant: users get a complete backend platform, not just a
workflow engine.

### The Land-and-Expand Strategy

1. **Land**: A team adopts cleat for blob storage (easiest adoption — every
   app needs file storage, and S3+metadata index is immediately useful).
2. **Expand**: They add event streams for audit logging (natural next step
   — they're already using the same API keys and tenant context).
3. **Expand further**: They adopt durable execution for the business processes
   that produce and consume those blobs and events (natural culmination —
   the workflows orchestrate the blobs and events they already use).

This inverts the adoption curve. Instead of "learn durable execution → set
up infrastructure → write workflows," it's "store a file → query it by tag →
oh, and there's a workflow engine too."

---

## 6. Implementation Effort

| Component | Effort | Dependencies |
|-----------|--------|--------------|
| Blob store (S3 backend) | 2-3 weeks | AWS SDK, presigned URL generation |
| Blob store (GCS backend) | 1 week | GCS SDK |
| Content-addressed dedup | 0.5 weeks | Blob store |
| Blob TTL/cleanup | 0.5 weeks | Background loop |
| Event store | 1 week | Schema + API |
| SSE streaming for events | 0.5 weeks | Event store |
| Standalone job queue | 0.5 weeks | Reuses workflow_instances |
| KV store | 0.5 weeks | Schema + API |
| **Total** | **6-8 weeks** | |

All components share the existing auth middleware, tenant context propagation,
RLS policies, Prometheus metrics, and API structure. They're additive, not
architectural changes.

---

## 7. Risks

### Scope creep
Adding 4 new subsystems to a pre-1.0 product risks diluting focus. The
workflow engine needs production hardening more than it needs a blob store.

**Mitigation**: Build the blob store first (highest standalone value), then
event store, then KV/queue (which are thin wrappers over existing tables).
Stop at any point if workflow hardening takes priority.

### S3 costs passthrough
Users pay their own S3 costs. This is good (cleat doesn't take the hit) but
means blob storage isn't "included" in the same way event streams are.

**Mitigation**: S3 costs are well-understood and cheap ($0.023/GB/month).
Any team considering cleat already uses S3.

### Doesn't solve the fundamental problem
The fundamental problem isn't features — it's production track record. Adding
a blob store doesn't make cleat more production-trusted for the core workflow
use case.

**Mitigation**: This is true but not fatal. The blob store is useful
independently. Even if a team never adopts workflows, they might adopt cleat
for blob storage + event streaming, and the product is better off for having
more users.
