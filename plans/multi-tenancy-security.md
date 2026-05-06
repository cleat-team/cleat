# Multi-Tenant Security Design for Cleat

## Context

Currently cleat has a single `namespace TEXT` column on `workflow_defs` and
`workflow_instances` with no authentication, no encryption, and no tenant
isolation. This design adds true multi-tenancy with column-level encryption,
compression, per-tenant key management, and an auth layer — allowing many orgs
to safely share one PostgreSQL instance.

## 1. Threat Model & Security Properties

### What we protect against
- **Database dump / backup theft**: all sensitive data is encrypted at rest
- **Accidental cross-tenant access**: row-level security + application-level
  tenant context enforcement
- **Insider threat (DB admin)**: cannot read encrypted columns without keys
- **SQL injection leading to data exfiltration**: encrypted columns are opaque

### What we don't protect against (out of scope)
- A compromised worker process (it holds the decryption keys)
- Side-channel attacks on the Go process
- Traffic sniffing between worker and DB (use TLS on the connection string)
- Malicious WASM workflows from the same tenant

### Security invariants
1. No plaintext tenant data ever appears in PostgreSQL outside of SQL-queryable
   columns
2. Every tenant has an independent key hierarchy — compromising one tenant's
   key material does not affect others
3. Encryption and decryption happen at the Go application layer, never in SQL
4. Key material never leaves the worker process unencrypted

---

## 2. Tenant Identity Model

### `tenant_id` — the fundamental isolation boundary

Replace the current `namespace TEXT` with `tenant_id UUID NOT NULL` on every
table. Every row in every table belongs to exactly one tenant.

```
workflow_defs:     tenant_id UUID NOT NULL
workflow_instances: tenant_id UUID NOT NULL
event_history:     tenant_id UUID NOT NULL
workflow_signals:  tenant_id UUID NOT NULL
workflow_schedules: tenant_id UUID NOT NULL
```

### Tenant metadata table (new)

```sql
CREATE TABLE tenants (
    tenant_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,          -- human-readable slug
    display_name TEXT NOT NULL,                -- UI label
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    suspended   BOOLEAN NOT NULL DEFAULT false,
    key_version INTEGER NOT NULL DEFAULT 1,    -- for key rotation
    quota_max_concurrent_workflows INTEGER NOT NULL DEFAULT 100,
    quota_max_history_per_workflow INTEGER NOT NULL DEFAULT 10000
);
```

`tenants` contains no secrets — it's plaintext, used for routing, quotas,
and key version tracking.

---

## 3. Encryption Architecture

### 3.1 Key Hierarchy

```
Tenant Master Key (TMK)
    └── stored in Vault/KMS, fetched at worker startup, cached in memory
        │
        └── Data Encryption Key (DEK) per column per row
                └── derived via HKDF-SHA256(TMK, column_name, tenant_id)
                    │
                    └── AES-256-GCM encrypt(plaintext, DEK, nonce)
                            └── stored as: version_byte || nonce || ciphertext || tag
```

### 3.2 Per-Column Key Derivation

Using a per-column DEK (derived from TMK) rather than a single per-tenant DEK
means compromising one column's DEK (e.g., via a side channel) does not
compromise other columns for the same tenant.

```go
func deriveDEK(tmk []byte, tenantID uuid.UUID, columnName string) []byte {
    info := []byte(fmt.Sprintf("cleat:v1:%s:%s", tenantID, columnName))
    return hkdf.Extract(sha256.New, tmk, info)[:32]
}
```

The `columnName` values are the PostgreSQL column names:
`wasm_bytes`, `input`, `result`, `request`, `response`, `signal_payload`,
`child_input`, `new_input`, `payload`, `query_state`, `event_history_payload`,
etc.

### 3.3 Wire Format

Each encrypted column is stored as `BYTEA` with this layout:

```
Byte 0:         version (0x01)
Bytes 1-12:     12-byte random nonce (AES-GCM standard)
Bytes 13-28:    16-byte authentication tag
Bytes 29+:      ciphertext
```

For columns currently stored as `JSONB` or `TEXT`, the column type changes to
`BYTEA`. The application layer handles JSON ↔ bytes conversion.

### 3.4 Encryption / Decryption API

```go
// crypto.go — new file: internal/crypto/crypto.go

type TenantCrypto struct {
    tenantID uuid.UUID
    tmk      []byte   // 32-byte AES-256 key
}

func NewTenantCrypto(tenantID uuid.UUID, tmk []byte) *TenantCrypto

// EncryptColumn encrypts plaintext for a specific column.
// If len(plaintext) > compressThreshold, compresses with zstd first.
func (tc *TenantCrypto) EncryptColumn(columnName string, plaintext []byte) ([]byte, error)

// DecryptColumn reverses EncryptColumn.
func (tc *TenantCrypto) DecryptColumn(columnName string, ciphertext []byte) ([]byte, error)
```

Compression threshold: columns > 1KB get zstd compression before encryption.
This covers all JSONB payload columns and WASM binaries.

### 3.5 Key Manager Interface

```go
// KeyManager abstracts key storage backends
type KeyManager interface {
    // GetTenantMasterKey returns the current TMK for a tenant.
    GetTenantMasterKey(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
    // RotateKey creates a new TMK version and returns it.
    // Old keys are retained for decryption of historical data.
    RotateKey(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
    // GetKeyByVersion returns a historical TMK for decryption.
    GetKeyByVersion(ctx context.Context, tenantID uuid.UUID, version int) ([]byte, error)
}
```

Implementations:
- `VaultKeyManager` — uses HashiCorp Vault transit engine
- `AWSKMSKeyManager` — uses AWS KMS with data key grants
- `FileKeyManager` — reads keys from a JSON file (dev/testing only)
- `EnvKeyManager` — reads a single key from `CLEAT_MASTER_KEY` env var (single-tenant dev)

---

## 4. Column Classification & Migration

### 4.1 Plaintext columns (stay as-is, used in SQL WHERE / ORDER BY / INDEX)

**workflow_defs**: `tenant_id`, `name`, `version`, `max_history_length`
**workflow_instances**: `tenant_id`, `id`, `def_name`, `def_version`, `status`,
  `assigned_to`, `heartbeat_at`, `next_wake_at`, `created_at`,
  `cancellation_requested`, `parent_workflow_id`
**event_history**: `tenant_id`, `workflow_id`, `step`
**workflow_signals**: `tenant_id`, `workflow_id`, `signal_name`
**workflow_schedules**: `tenant_id`, `name`, `enabled`, `next_run_at`
**tenants**: all columns (metadata table)

### 4.2 Encrypted columns (type changes to BYTEA, app-layer encrypt/decrypt)

| Table | Column | Current Type | Encrypted? | Compressed? |
|-------|--------|-------------|------------|-------------|
| workflow_defs | wasm_bytes | BYTEA | Yes | Yes (zstd) |
| workflow_defs | entry_points | TEXT[] | Yes | No |
| workflow_instances | input | JSONB | Yes | Yes |
| workflow_instances | result | JSONB | Yes | Yes |
| workflow_instances | error_msg | TEXT | Yes | No |
| workflow_instances | cancellation_reason | TEXT | Yes | No |
| workflow_instances | query_state | JSONB | Yes | Yes |
| event_history | request | JSONB | Yes | Yes |
| event_history | response | JSONB | Yes | Yes |
| event_history | signal_payload | JSONB | Yes | Yes |
| event_history | child_input | JSONB | Yes | Yes |
| event_history | new_input | JSONB | Yes | Yes |
| event_history | service | TEXT | Yes | No |
| event_history | operation | TEXT | Yes | No |
| event_history | error | TEXT | Yes | No |
| event_history | event_type | TEXT | Yes | No |
| workflow_signals | payload | JSONB | Yes | Yes |
| workflow_schedules | input | JSONB | Yes | Yes |
| workflow_schedules | def_name | TEXT | Yes | No |
| workflow_schedules | entry_point | TEXT | Yes | No |
| workflow_schedules | cron_expression | TEXT | Yes | No |

### 4.3 Unchanged columns (timestamps with no sensitive data)

`created_at`, `completed_at`, `delivered_at`, `last_run_at`, `duration_ms`,
`timeout_ms`, `heartbeat_at`, `next_wake_at` — these are timestamps/durations
with no workflow data content. They stay plaintext as `TIMESTAMPTZ` / `BIGINT`.

---

## 5. Authentication & Authorization

### 5.1 API Key Model

Each tenant gets one or more API keys. Keys are stored hashed (SHA-256) in a
new `tenant_api_keys` table:

```sql
CREATE TABLE tenant_api_keys (
    key_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(tenant_id),
    key_hash    BYTEA NOT NULL,       -- SHA-256(key)
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE INDEX idx_api_keys_hash ON tenant_api_keys(key_hash) WHERE revoked_at IS NULL;
```

### 5.2 HTTP Middleware

```go
// middleware.go — new file: internal/auth/middleware.go

func TenantAuthMiddleware(store TenantStore) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // Support: Authorization: Bearer cleat_sk_<key>
            // Support: X-Cleat-API-Key: <key>
            key := extractAPIKey(r)
            if key == "" {
                http.Error(w, `{"error":"missing API key"}`, 401)
                return
            }
            keyHash := sha256.Sum256([]byte(key))
            tenantID, err := store.LookupTenantByKeyHash(r.Context(), keyHash[:])
            if err != nil {
                http.Error(w, `{"error":"invalid API key"}`, 401)
                return
            }
            ctx := context.WithValue(r.Context(), tenantIDKey{}, tenantID)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### 5.3 Tenant Context Propagation

A `tenantID` value flows through `context.Context` from HTTP handler → DB
store → engine → host functions. Every `WorkflowStore` method checks that
the provided workflow IDs belong to the context's tenant.

```go
type tenantIDKey struct{}

func TenantIDFromContext(ctx context.Context) (uuid.UUID, error) {
    tid, ok := ctx.Value(tenantIDKey{}).(uuid.UUID)
    if !ok || tid == uuid.Nil {
        return uuid.Nil, ErrNoTenant
    }
    return tid, nil
}
```

---

## 6. PostgreSQL Row-Level Security (Defense in Depth)

Application-layer encryption is the primary security control, but PostgreSQL
RLS provides a second layer — if a SQL injection bypasses the application,
RLS ensures the query can only see the current tenant's rows.

```sql
ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_defs ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_schedules ENABLE ROW LEVEL SECURITY;

-- The worker sets tenant_id at session start
CREATE POLICY tenant_isolation ON workflow_instances
    USING (tenant_id = current_setting('cleat.tenant_id')::uuid);

-- Apply identical policy to all 5 tables
```

The worker sets the session variable at connection time:
```go
_, err := db.ExecContext(ctx, "SET cleat.tenant_id = $1", tenantID)
```

For the single-tenant-worker model, this is set once per `*sql.DB`. For a
multi-tenant worker, each goroutine uses a connection from a pool and sets it
before queries.

---

## 7. Worker Multi-Tenancy Modes

### Mode A: Single-tenant worker (current model, simplest)

Each worker deployment connects as one tenant. `--tenant-id <uuid>` flag.
One `*sql.DB`, one TMK loaded at startup, one RLS session variable.
Separate K8s Deployments per tenant.

### Mode B: Multi-tenant worker (cost-amortized)

A single worker deployment handles multiple tenants. The worker maintains:
- A `map[uuid.UUID]*TenantCrypto` cache of loaded tenant keys
- A `map[uuid.UUID]*sql.DB` of per-tenant connection pools (optional — can use
  one pool with RLS switching, but per-tenant pools provide connection
  isolation)
- The dispatch loop polls across all active tenants in round-robin order
- Quotas enforced per tenant (max concurrent workflows, max history length)

```go
type MultiTenantWorker struct {
    tenants     map[uuid.UUID]*TenantContext
    tenantOrder []uuid.UUID
    mu          sync.RWMutex
}

type TenantContext struct {
    TenantID    uuid.UUID
    Crypto      *TenantCrypto
    DB          *sql.DB           // optional per-tenant pool
    Store       *PostgresStore    // scoped to this tenant
    Concurrency int               // goroutines currently running for this tenant
    Quota       TenantQuota
}
```

The dispatch loop:
```go
func (w *MultiTenantWorker) dispatchLoop() {
    for {
        w.mu.RLock()
        for _, tid := range w.tenantOrder {
            tc := w.tenants[tid]
            if tc.Concurrency >= tc.Quota.MaxConcurrentWorkflows {
                continue
            }
            wf, err := tc.Store.ClaimWorkflow(ctx, workerID, tid)
            if wf != nil {
                tc.Concurrency++
                go w.executeWorkflow(tc, wf)
            }
        }
        w.mu.RUnlock()
        time.Sleep(pollInterval)
    }
}
```

---

## 8. Connection Pooling & Resource Isolation

### Problem

A single `*sql.DB` pool shared by all tenants means one tenant can exhaust
connections and starve others.

### Solution: Per-tenant connection pool limits

```go
func newTenantDBPool(baseConnStr string, tenantID uuid.UUID, maxConns int) (*sql.DB, error) {
    db, err := sql.Open("postgres", baseConnStr)
    if err != nil {
        return nil, err
    }
    db.SetMaxOpenConns(maxConns)      // e.g., 20 per tenant
    db.SetMaxIdleConns(maxConns / 2)  // e.g., 10 per tenant
    db.SetConnMaxLifetime(5 * time.Minute)
    return db, nil
}
```

For a cost-amortized deployment with 10 tenants on a single PG instance with
`max_connections = 200`:
- Each tenant gets 15 max open connections (150 total)
- 50 connections reserved for system/superuser/maintenance

---

## 9. Key Rotation

### Rotation process (zero-downtime)

1. Admin calls `KeyManager.RotateKey(ctx, tenantID)` — creates TMK v2
2. `tenants.key_version` is incremented to 2
3. **New writes** use TMK v2 (stored with version byte `0x02`)
4. **Old data** retains TMK v1 ciphertext (version byte `0x01`)
5. `TenantCrypto.DecryptColumn()` reads the version byte and loads the correct
   historical key via `KeyManager.GetKeyByVersion()`
6. Optional background re-encryption job reads v1 rows and rewrites with v2

### Key version byte in ciphertext

The first byte of every encrypted BYTEA is the key version. This allows the
decryption path to select the correct key without a separate column.

---

## 10. Performance Impact Analysis

| Operation | Plaintext | Encrypted | Overhead |
|-----------|-----------|-----------|----------|
| WASM load (2 MB) | ~2ms | ~5ms (decrypt + decompress) | +3ms, once per version (cached) |
| Event history load (50 steps, 5KB each) | ~1ms | ~8ms (50 decrypts) | +7ms per replay |
| Write event (5KB JSONB) | ~0.3ms | ~2ms (compress + encrypt) | +1.7ms |
| DB storage (WASM 2 MB) | 2 MB | ~600 KB (zstd ~70% + 29 bytes GCM overhead) | -70% storage |
| DB storage (JSONB 10 KB) | 10 KB | ~4 KB (zstd ~60% + 29 bytes GCM overhead) | -60% storage |

Key observations:
- WASM binaries compress extremely well (70%+ with zstd), so encryption
  actually **reduces** storage even with the GCM overhead.
- Event history replay latency increases by ~7ms per 50 events — this is in
  the noise compared to WASM execution time (tens to hundreds of ms).
- Write latency increase of ~1.7ms per event is acceptable — the PostgreSQL
  INSERT itself is ~1-2ms.

---

## 11. Implementation Plan

### Phase 1: Foundation (Week 1-2)

**Files to create:**
- `internal/crypto/crypto.go` — `TenantCrypto`, `deriveDEK`, encrypt/decrypt
- `internal/crypto/crypto_test.go` — round-trip tests, key rotation tests,
  compression tests
- `internal/crypto/key_manager.go` — `KeyManager` interface, `FileKeyManager`,
  `EnvKeyManager`
- `internal/crypto/key_manager_test.go`

**Files to modify:**
- `go.mod` — add `golang.org/x/crypto` (HKDF), `github.com/klauspost/compress`
  (zstd)

**Verification:**
- `go test ./internal/crypto/...`
- Benchmarks: `BenchmarkEncryptWASM`, `BenchmarkDecryptWASM`,
  `BenchmarkEncryptJSONB`

### Phase 2: Schema Migration (Week 2-3)

**New migration file:** `migrations/002_multi_tenant.sql`

```sql
-- 1. Create tenants table
CREATE TABLE tenants (...);

-- 2. Add tenant_id to all existing tables
ALTER TABLE workflow_defs ADD COLUMN tenant_id UUID;
ALTER TABLE workflow_instances ADD COLUMN tenant_id UUID;
ALTER TABLE event_history ADD COLUMN tenant_id UUID;
ALTER TABLE workflow_signals ADD COLUMN tenant_id UUID;
ALTER TABLE workflow_schedules ADD COLUMN tenant_id UUID;

-- 3. Create a default tenant for existing data
INSERT INTO tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default Tenant');

-- 4. Backfill existing rows
UPDATE workflow_defs SET tenant_id = '00000000-0000-0000-0000-000000000001'
    WHERE tenant_id IS NULL;
-- ... repeat for all 5 tables

-- 5. Make tenant_id NOT NULL
ALTER TABLE workflow_defs ALTER COLUMN tenant_id SET NOT NULL;
-- ... repeat

-- 6. Add composite indexes including tenant_id
CREATE INDEX idx_defs_tenant_name ON workflow_defs(tenant_id, name, version DESC);
CREATE INDEX idx_inst_tenant_status ON workflow_instances(tenant_id, status, next_wake_at)
    WHERE status = 'ready';
-- ... etc.

-- 7. Enable RLS
ALTER TABLE workflow_defs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_defs ON workflow_defs
    USING (tenant_id = current_setting('cleat.tenant_id')::uuid);
-- ... repeat for all tables

-- 8. Create tenant_api_keys table
CREATE TABLE tenant_api_keys (...);

-- 9. Drop old namespace column (replaced by tenant_id)
ALTER TABLE workflow_defs DROP COLUMN namespace;
ALTER TABLE workflow_instances DROP COLUMN namespace;
```

### Phase 3: Application Encryption (Week 3-4)

**Files to modify:**
- `internal/host/db.go` — `PostgresStore` methods: encrypt on write, decrypt
  on read. Add `crypto *TenantCrypto` field. Change query parameter types
  from `string`/`[]byte` to handle `BYTEA` encrypted columns.
- `internal/host/engine.go` — pass `TenantCrypto` to store operations.
- `cmd/durable-worker/main.go` — load tenant keys at startup, initialize
  `TenantCrypto`, pass to `PostgresStore`.

**Key changes in `PostgresStore`:**
```go
type PostgresStore struct {
    db     *sql.DB
    crypto *crypto.TenantCrypto  // new
}

func (s *PostgresStore) LoadEventHistory(ctx context.Context, workflowID string) ([]Event, error) {
    rows, err := s.db.QueryContext(ctx, `SELECT ... FROM event_history WHERE workflow_id = $1 ORDER BY step`, workflowID)
    // ... for each row:
    for rows.Next() {
        var encRequest, encResponse []byte  // now BYTEA, not JSONB
        rows.Scan(..., &encRequest, &encResponse, ...)
        // decrypt
        event.Request, _ = s.crypto.DecryptColumn("request", encRequest)
        event.Response, _ = s.crypto.DecryptColumn("response", encResponse)
    }
}
```

### Phase 4: Auth Middleware (Week 4)

**Files to create:**
- `internal/auth/middleware.go` — HTTP middleware
- `internal/auth/middleware_test.go`
- `internal/auth/tenant_store.go` — `LookupTenantByKeyHash`

**Files to modify:**
- `cmd/durable-worker/main.go` — wrap HTTP mux with `TenantAuthMiddleware`
- `cmd/durable/main.go` — add `--tenant-id` flag for CLI operations (deploy,
  schedule, versions, rollback) — these bypass API key auth and use direct DB
  access with the tenant ID flag

### Phase 5: CLI & Worker Multi-Tenant Support (Week 5)

**Files to modify:**
- `cmd/durable-worker/main.go` — add `--multi-tenant` flag, `--tenant-key-file`
  flag. In multi-tenant mode, load all tenants from config and use
  `MultiTenantWorker` dispatch loop.
- `cmd/durable/main.go` — add `--tenant-id` flag to deploy/schedule/versions/
  rollback subcommands. Remove `--namespace` flag.

**New config format:**
```json
// tenants.json — loaded via --tenant-key-file
{
  "tenants": [
    {
      "tenant_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
      "name": "acme-corp",
      "key_source": {
        "type": "vault",
        "path": "secret/cleat/tenants/acme-corp"
      },
      "quota": {
        "max_concurrent_workflows": 100,
        "max_history_per_workflow": 10000
      },
      "db_pool_max_conns": 15
    }
  ]
}
```

### Phase 6: Testing & Hardening (Week 6)

**New test files:**
- `internal/host/multi_tenant_test.go` — end-to-end: two tenants, same DB,
  verify isolation
- `internal/crypto/rotation_test.go` — key rotation with concurrent reads
- `internal/auth/integration_test.go` — HTTP endpoints with auth middleware

**Penetration testing checklist:**
- Verify encrypted columns are opaque in `pg_dump` output
- Verify RLS policies block cross-tenant reads even with raw SQL
- Verify key rotation doesn't break in-flight workflows
- Verify a compromised tenant key can't decrypt another tenant's data
- Performance test: 10 tenants × 100 concurrent workflows each

---

## 12. Rollout Strategy

### Backward compatibility

The migration adds `tenant_id` columns with a default tenant. Existing
single-tenant deployments continue to work unchanged — all rows belong to
the default tenant. Encryption is opt-in via `--encrypt-at-rest` flag.

### Gradual encryption rollout

1. Deploy schema migration (Phase 2) — all data still plaintext, tenant_id
   added
2. Deploy application changes (Phase 3) with encryption disabled
3. Enable encryption per-tenant via `UPDATE tenants SET encrypt_at_rest = true`
4. Background job re-encrypts existing rows for that tenant
5. Once all tenants are encrypted, remove plaintext code paths

### Estimated timeline

| Phase | Duration | Effort |
|-------|----------|--------|
| 1: Crypto foundation | 1-2 weeks | 1 engineer |
| 2: Schema migration | 1 week | 1 engineer |
| 3: Application encryption | 1-2 weeks | 1 engineer |
| 4: Auth middleware | 1 week | 1 engineer |
| 5: CLI & worker multi-tenant | 1 week | 1 engineer |
| 6: Testing & hardening | 1 week | 1 engineer |
| **Total** | **6-8 weeks** | | 

---

## 13. Cost Amortization Analysis

Example: 10 tenants on a single PostgreSQL instance

| Resource | Single-Tenant (10 instances) | Multi-Tenant (1 instance) | Savings |
|----------|------|------|---------|
| PostgreSQL instances | 10 × $20/mo = $200/mo | 1 × $100/mo | **50%** |
| K8s worker pods | 10 × 3 replicas = 30 pods | 1 × 5 replicas = 5 pods | **83%** |
| Vault/KMS operations | 10 separate configs | 1 config, 10 keys | Same |
| Admin overhead | 10 DBs to manage | 1 DB to manage | **90%** |

The encryption overhead (~2ms per event write, ~7ms per history load) is
negligible compared to WASM execution time (typically 10-500ms per step).
Storage actually **decreases** due to zstd compression on JSONB and WASM
columns.

---

## 14. Verification

### Unit tests
- `crypto_test.go`: round-trip encrypt/decrypt, compression, key rotation,
  wrong-key detection, tampered ciphertext detection
- `middleware_test.go`: valid key, invalid key, revoked key, missing key,
  malformed header

### Integration tests
- `multi_tenant_test.go`: start workflows for tenant A and tenant B, verify
  tenant A cannot see tenant B's workflows, verify encrypted columns differ
  for identical plaintext in different tenants (different DEKs)

### Manual verification
- `pg_dump` the database and confirm encrypted columns are binary garbage
- Run `SELECT * FROM workflow_instances` as superuser and confirm encrypted
  columns are unreadable without keys
- Verify RLS prevents `SELECT * FROM workflow_instances` without setting
  `cleat.tenant_id`
