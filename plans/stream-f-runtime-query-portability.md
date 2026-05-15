# Stream F: Runtime Query Portability

May 2026 — plan for making plugin runtime queries work across PostgreSQL,
MySQL 8.0+, and SQL Server 2017+.

---

## Context

Stream A added `UpMySQL`/`UpMSSQL` fields to `plugin.Migration`, making DDL
dialect-aware. Streams B and C will translate the migration SQL. But **runtime
queries** — the SELECTs, INSERTs, UPDATEs, and DELETEs in plugin Go code — are
still hardcoded to PostgreSQL syntax. An audit of all 17 plugins found 200+
parameter placeholder violations (`$1` → needs `?` on MySQL, `@p1` on MSSQL)
and ~30 structurally PostgreSQL-specific queries (`ON CONFLICT`, `RETURNING`,
JSONB operators, interval arithmetic).

This stream adds a lightweight two-layer portability system to plugin runtime
queries without introducing a query builder, ORM, or new dependencies.

---

## Stream Map

```
Stream F.0: Test compilation rot fix    ── prerequisite (no parallelism)
Stream F.1: Rebind + Query types         ── core API (blocks everything)
Stream F.2: Dialect in Environment       ── one-line struct change
Stream F.3: Per-plugin query porting     ── 17 plugins (parallel after F.1+F.2)
Stream F.4: Hardcoded driver fixes       ── 3 CLI commands (parallel with F.3)
Stream F.5: CI behavioral matrix         ── after F.3
```

---

## F.0: Fix Pre-Existing Test Compilation Rot

**Goal:** Make all 17 plugin test suites compile before touching runtime queries.

### Problem

The `plugin.PluginDB.Begin()` method signature was changed to require a
`context.Context` parameter. 15 plugin test files still pass `*sql.DB` directly
where `plugin.PluginDB` is expected, or reference `host.SQLDBAdapter` without
importing the `host` package. These tests have never compiled since the
interface change.

### Fix

Two mechanical changes per affected file:

1. **Missing `host` import** — add `"github.com/cleat-team/cleat/internal/host"` to
   the import block.

2. **`*sql.DB` → `PluginDB` mismatch** — wrap `*sql.DB` values in
   `&host.SQLDBAdapter{DB: db}`. This applies to:
   - `sql.OpenDB(&fakeConnector{...})` return values
   - `&sql.DB{}` literals
   - Raw `db` variables passed to plugin struct fields or `Environment.DB`

### Affected plugins (15)

auditlog, blobstore, datadogexport, eventstore, eventtriggers, jobqueue,
kafkaconnect, notifications, oauthprovider, pagerdutyalert, pgvector,
ratelimiter, scheduledbackup, scheduler, slacknotify, webhookingest

(kvstore and featureflags were fixed during Stream D.)

### Validation

```bash
for dir in plugins/*/; do
  go test -c -o /dev/null "./$dir" 2>&1 || echo "FAIL: $dir"
done
```

Every plugin must produce a zero-exit-code compilation. Actual test PASS/FAIL
behavior is addressed in F.3.

**F.0 total: ~0.5 day**

---

## F.1: Rebind Function and Query Struct

**Goal:** Add two types and one function to `internal/plugin/` for runtime
query portability.

**Files:** `internal/plugin/query.go` (new)

### F.1.1 `Rebind(query string, d Dialect) string`

Translates PostgreSQL `$N` parameter placeholders to the dialect-appropriate
form. Also handles the `now()` → `SYSUTCDATETIME()` substitution for MSSQL.

```go
package plugin

import "regexp"

var dollarRE = regexp.MustCompile(`\$(\d+)`)

func Rebind(query string, d Dialect) string {
    q := query
    switch d {
    case DialectMySQL:
        // MySQL uses positional ? for all params. Replace each $N with ?.
        q = dollarRE.ReplaceAllString(q, "?")
    case DialectMSSQL:
        // MSSQL uses @p1, @p2, ...
        q = dollarRE.ReplaceAllString(q, "@p$1")
    }
    // now() → SYSUTCDATETIME() on MSSQL. MySQL supports now() natively.
    if d == DialectMSSQL {
        q = nowRE.ReplaceAllString(q, "SYSUTCDATETIME()")
    }
    return q
}

var nowRE = regexp.MustCompile(`(?i)\bnow\s*\(\s*\)`)
```

Usage in plugin code:

```go
// Before
err := p.db.QueryRow(ctx,
    "SELECT value FROM kv_store WHERE tenant_id = $1 AND key = $2",
    tid, key).Scan(&val)

// After
err := p.db.QueryRow(ctx,
    plugin.Rebind("SELECT value FROM kv_store WHERE tenant_id = $1 AND key = $2", p.dialect),
    tid, key).Scan(&val)
```

This single function eliminates the 200+ placeholder violations across all 17
plugins. It is idempotent — calling it on an already-rebound query is safe
(the regex won't match `?` or `@pN`).

### F.1.2 `Query` struct

For queries that differ structurally (upserts, RETURNING, JSON operators), a
lightweight struct that mirrors the `Migration` pattern:

```go
// Query holds dialect-specific variants of a runtime SQL query.
// Default is PostgreSQL. MySQL and MSSQL are optional overrides —
// empty means the query is unsupported on that backend (caller should
// check and return a clear error, not crash).
type Query struct {
    Default string // required — PostgreSQL
    MySQL   string // optional
    MSSQL   string // optional
}

// For returns the query string for the given dialect.
func (q Query) For(d Dialect) string {
    switch d {
    case DialectMySQL:
        if q.MySQL != "" {
            return q.MySQL
        }
    case DialectMSSQL:
        if q.MSSQL != "" {
            return q.MSSQL
        }
    }
    return q.Default
}
```

Usage:

```go
var upsertKV = plugin.Query{
    Default: `INSERT INTO kv_store (...) VALUES ($1, $2, ...)
               ON CONFLICT (tenant_id, key) DO UPDATE SET ... RETURNING version`,
    MySQL:   `INSERT INTO kv_store (...) VALUES (?, ?, ...)
               ON DUPLICATE KEY UPDATE ...; SELECT LAST_INSERT_ID()`,
    MSSQL:   `MERGE kv_store AS target USING (VALUES (@p1, @p2, ...))
               AS source (...) ON target.tenant_id = source.tenant_id
               AND target.key = source.key
               WHEN MATCHED THEN UPDATE SET ...
               WHEN NOT MATCHED THEN INSERT ...
               OUTPUT INSERTED.version;`,
}

// At call site:
err := p.db.QueryRow(ctx, upsertKV.For(p.dialect), args...).Scan(&v)
```

Note: `Query.For()` returns the raw dialect SQL. Callers that use `$N`
placeholders in the `Default` field must still call `Rebind()` on the result,
or write the dialect variants with native placeholders. The convention is:
- `Default` uses PostgreSQL placeholders (`$1`, `$2`, ...)
- `MySQL` uses `?` placeholders
- `MSSQL` uses `@p1`, `@p2`, ... placeholders

The caller does `Rebind(q.For(d), d)` and `Rebind` is a no-op for `Default`
and `MySQL` (already has native placeholders), and for `MSSQL` it's also a
no-op since the variant already uses `@pN`.

Actually, simpler: variants should use `$N` placeholders too, and callers
always run `Rebind()`. `Rebind` on MySQL converts `$N` → `?`; on MSSQL
converts `$N` → `@pN`. This way only one placeholder format exists in source.

**F.1 total: ~0.5 day**

---

## F.2: Add Dialect to plugin.Environment

**Goal:** Plugins know which backend they're running against at runtime.

**File:** `internal/plugin/plugin.go`

### Change

```go
type Environment struct {
    DB          PluginDB
    Mux         *http.ServeMux
    Config      json.RawMessage
    Logger      *slog.Logger
    TenantID    uuid.UUID
    Done        <-chan struct{}
    Dialect     Dialect   // NEW
    // ... existing fields
}
```

### Caller update

In `cmd/cleat-worker/main.go`, set `env.Dialect = plugin.Dialect(factory.Dialect())`
when constructing the environment. This is a one-line addition at the call site.

### Plugin convention

Plugins store the dialect during `Init()`:

```go
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
    p.db = env.DB
    p.dialect = env.Dialect
    // ...
}
```

**F.2 total: ~0.25 day**

---

## F.3: Per-Plugin Query Porting

**Goal:** Apply `Rebind()` to every runtime query and add `Query` variants for
structurally different queries. Plugins with no SQL remain unchanged.

### Tiers

**Tier 1 — Rebind only (8 plugins):** These plugins only have parameter
placeholder violations and `now()` calls. No structural query differences.

| Plugin | Files | Lines changed |
|--------|-------|---------------|
| auditlog | background.go, middleware.go, routes.go | ~15 |
| datadogexport | background.go, routes.go | ~12 |
| featureflags | host_functions.go, routes.go | ~12 |
| jobqueue | background.go, routes.go | ~10 |
| kafkaconnect | host_functions.go, routes.go | ~5 |
| notifications | background.go, host_functions.go, routes.go | ~18 |
| oauthprovider | middleware.go, routes.go | ~10 |
| pagerdutyalert | host_functions.go, routes.go | ~8 |
| slacknotify | host_functions.go, routes.go | ~8 |

Each change is mechanical: wrap the query string in `plugin.Rebind(..., p.dialect)`.

**Tier 2 — Rebind + Query struct (5 plugins):** These plugins have
structurally different queries (upserts, RETURNING, JSON operators,
INTERVAL arithmetic) that need per-dialect variants.

| Plugin | Structurally different queries | Count |
|--------|-------------------------------|-------|
| blobstore | upsert (×3), RETURNING delete (×2), JSONB `@>` | 6 |
| kvstore | upsert + RETURNING (×2) | 2 |
| eventstore | `::jsonb` cast, RETURNING, `make_interval()` | 3 |
| eventtriggers | upsert (×2), RETURNING, `INTERVAL` | 4 |
| ratelimiter | upsert | 1 |
| webhookingest | `INTERVAL` | 1 |
| scheduledbackup | `FOR UPDATE SKIP LOCKED` | 1 |
| scheduler | `FOR UPDATE SKIP LOCKED` | 1 |

Each plugin defines package-level `Query` variables for its structurally
different queries and uses `q.For(p.dialect)` at the call site. All other
queries get `Rebind()`.

**Tier 3 — No changes (2 plugins):** dag and llm have no database access.

### Workflow per plugin

1. Add `dialect plugin.Dialect` field to plugin struct.
2. Store `env.Dialect` in `Init()`.
3. For every SQL string literal in non-test, non-migration `.go` files:
   - Wrap with `plugin.Rebind(..., p.dialect)`.
   - If the query uses `ON CONFLICT`, `RETURNING`, `::type` casts, JSONB
     operators, `INTERVAL`, `make_interval`, or `FOR UPDATE SKIP LOCKED`,
     extract it into a package-level `plugin.Query` variable with all three
     variants.
4. Compile: `go build ./plugins/<name>/... && go test -c -o /dev/null ./plugins/<name>/`
5. Run fake-driver behavioral tests to verify no regressions.
6. For plugins wired up to multi-backend testing (kvstore, featureflags), run
   against a real PostgreSQL instance to verify end-to-end.

### Translation reference for Query variants

| PostgreSQL | MySQL | MSSQL |
|-----------|-------|-------|
| `ON CONFLICT (cols) DO UPDATE SET c = EXCLUDED.c` | `ON DUPLICATE KEY UPDATE c = VALUES(c)` | `MERGE ... WHEN MATCHED THEN UPDATE ... WHEN NOT MATCHED THEN INSERT` |
| `ON CONFLICT (cols) DO NOTHING` | `INSERT IGNORE INTO ...` | `MERGE ... WHEN NOT MATCHED THEN INSERT` |
| `RETURNING col` | Separate `SELECT LAST_INSERT_ID()` or row count | `OUTPUT INSERTED.col` |
| `::jsonb` | `CAST(x AS JSON)` | n/a (use NVARCHAR(MAX)) |
| `@>` (JSONB containment) | `JSON_CONTAINS(col, val)` | `EXISTS (SELECT 1 FROM OPENJSON(col) WHERE ...)` |
| `make_interval(days => $1)` | `DATE_SUB(NOW(), INTERVAL ? DAY)` | `DATEADD(day, -@p1, SYSUTCDATETIME())` |
| `INTERVAL '10 seconds'` | `INTERVAL 10 SECOND` | `DATEADD(second, -10, SYSUTCDATETIME())` |
| `FOR UPDATE SKIP LOCKED` | `FOR UPDATE SKIP LOCKED` (MySQL 8.0+) | `WITH (UPDLOCK, READPAST, ROWLOCK)` hint |
| `now()` | `NOW()` or `NOW(6)` | `SYSUTCDATETIME()` |

**F.3 total: ~3 days**

---

## F.4: Hardcoded Driver Fixes

**Goal:** Three plugins have CLI commands that call `sql.Open("postgres", dsn)`
directly, bypassing the multi-DB store factory. These must accept a driver name
parameter or use the factory pattern.

**Affected files:**
- `plugins/jobqueue/commands.go:50`
- `plugins/scheduledbackup/commands.go:63`
- `plugins/scheduler/commands.go:57,154,199`

### Fix

Replace `sql.Open("postgres", dsn)` with a configurable driver name. The
simplest approach: read the driver from an environment variable or accept it
as a CLI flag. For consistency with the test infrastructure, use the same
env var convention:

```go
driver := os.Getenv("CLEAT_DB_DRIVER")
if driver == "" {
    driver = "postgres"
}
db, err := sql.Open(driver, dsn)
```

Alternatively, if the commands have access to a `StoreFactory`, delegate
connection creation to the factory. This is cleaner but requires plumbing
the factory through to the command layer.

**F.4 total: ~0.5 day**

---

## F.5: CI Behavioral Matrix

**Goal:** Expand the multi-db CI workflow to run plugin behavioral tests
against all three backends.

### F.5.1 Activate the D.2 multi-backend tests

The kvstore and featureflags multi-backend tests created in Stream D
(`*_multidb_test.go`) currently skip MySQL and MSSQL because the plugins
lack `UpMySQL`/`UpMSSQL` migrations. Once Streams B and C land, these
tests will run against all backends. After Stream F makes runtime queries
portable, these tests will exercise the full path: migration → query →
result verification.

### F.5.2 Extend CI to more plugins

As more plugins get wired up with `*_multidb_test.go` files, add them to
the CI matrix. Start with the Tier 2 plugins (blobstore, eventstore,
eventtriggers, ratelimiter) since they have the most complex query patterns.

### F.5.3 CI job

Update `.github/workflows/multi-db-ci.yml` to run:

```yaml
- name: Run plugin behavioral tests (all backends)
  run: go test -race -count=1 -timeout=600s -run 'MultiBackend' ./plugins/...
```

This matches the `*MultiBackend` naming convention established in Stream D.

**F.5 total: ~0.5 day**

---

## Effort Summary

| Stream | Description | Duration |
|--------|-------------|----------|
| F.0 | Fix pre-existing test compilation rot (15 plugins) | 0.5 day |
| F.1 | Rebind + Query types in internal/plugin | 0.5 day |
| F.2 | Dialect on Environment | 0.25 day |
| F.3 | Per-plugin query porting (17 plugins) | 3 days |
| F.4 | Hardcoded driver fixes (3 CLI commands) | 0.5 day |
| F.5 | CI behavioral matrix | 0.5 day |

**Total: ~5.25 days** for one developer. With parallel AI assistance, 2-3 days.

---

## Dependencies

| Stream | Depends on |
|--------|-----------|
| F.0 | Stream D (test infrastructure exists) |
| F.1 | Stream A (Dialect type exists) |
| F.2 | Stream A, F.1 |
| F.3 | F.0, F.1, F.2, Stream B, Stream C |
| F.4 | F.0 |
| F.5 | F.3 |

Streams B and C (MySQL/MSSQL migration translations) must be complete before
F.3 can be validated end-to-end — without the DDL, tables don't exist and
runtime queries have nothing to query.

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `Rebind` silently corrupts queries with `$` in non-placeholder contexts (e.g., JSON `'$schema'`) | Low | Medium | `$` followed by non-digit is not matched by the regex; audit all `$` usage in plugin SQL strings |
| MySQL `ON DUPLICATE KEY UPDATE` semantics differ from PG `ON CONFLICT` for multi-column unique constraints | Medium | Medium | Test each upsert query individually; `VALUES()` in MySQL always refers to the insert value, matching `EXCLUDED` behavior |
| MSSQL `MERGE` is more verbose and error-prone than PG upsert | Medium | Medium | Template the MERGE pattern; review each MSSQL variant carefully in code review |
| `FOR UPDATE SKIP LOCKED` works on MySQL 8.0+ but requires InnoDB | Low | Low | MySQL 8.0 is the minimum target (per plan); InnoDB is default |
| Plugin forgets to call `Rebind()` on a new query added after Stream F | Medium | Low | CI runs behavioral tests against all three backends; missing Rebind causes immediate failure on MySQL |
| MSSQL `OUTPUT INSERTED.col` returns rows differently than PG `RETURNING` for multi-row inserts | Low | Medium | All current RETURNING usage is single-row; flag in review checklist for future multi-row inserts |
| Hardcoded `sql.Open("postgres", ...)` in CLI commands missed during F.4 | Low | High | Grep for `sql.Open` across entire codebase before closing F.4 |
