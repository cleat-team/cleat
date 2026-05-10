# Plugin Multi-Database Plan

May 2026 — plan for making cleat plugins database-agnostic across PostgreSQL,
MySQL 8.0+, and SQL Server 2017+.

---

## Scope

18 plugins ship PostgreSQL-specific migration SQL embedded in Go files. Only
**pgvector** is inherently PostgreSQL-coupled (it uses the `pgvector` extension).
The other 17 plugins define ~26 migrations creating ~28 tables — all generic
config/operational tables that could work against any backend.

Currently, pointing a MySQL or MSSQL deployment at these plugins causes the
migration step to fail with SQL syntax errors (`JSONB`, `TIMESTAMPTZ`, `UUID`,
`gen_random_uuid()`).

This plan adds dialect-aware migration fields to the plugin `Migration` type and
translates the 17 general-purpose plugins. It does NOT touch pgvector (which
remains PostgreSQL-only) and does NOT build a migration DSL.

---

## Stream Map

```
Stream A: Migration API           ────────────┐
Stream B: MySQL translations       ───────────┤  These two run in parallel after A
Stream C: MSSQL translations       ───────────┘
Stream D: Testing and CI           ── after B+C
Stream E: Documentation            ── throughout
Stream F: Runtime query portability ── after B+C+D
```

Stream F is defined in `plans/stream-f-runtime-query-portability.md`.

---

## Stream A: Migration API

**Goal:** Add optional dialect-specific SQL fields to `plugin.Migration` and
update `RunMigrations` to select the right one. Plugins without dialect-specific
SQL are skipped with a clear warning rather than crashing.

**Files:** `internal/plugin/plugin.go`, `internal/plugin/migration.go`

### A.1 Extend Migration struct

```go
type Migration struct {
    Version   int
    Up        string // required — default SQL (PostgreSQL). Backward-compatible.
    UpMySQL   string // optional — MySQL DDL. Empty → plugin is PG-only for this version.
    UpMSSQL   string // optional — MSSQL DDL.
    Down      string // optional rollback (unchanged)
}
```

`Up` keeps its existing semantics (the default). Existing plugins compile without
changes. The new fields are strictly additive.

### A.2 Update RunMigrations dispatch

In `RunMigrations`, select the dialect-appropriate SQL:

```go
sql := m.Up
switch dialect {
case DialectMySQL:
    if m.UpMySQL != "" {
        sql = m.UpMySQL
    } else {
        log.Printf("[plugin] %s v%d: no MySQL migration — skipping", name, m.Version)
        continue
    }
case DialectMSSQL:
    if m.UpMSSQL != "" {
        sql = m.UpMSSQL
    } else {
        log.Printf("[plugin] %s v%d: no MSSQL migration — skipping", name, m.Version)
        continue
    }
}
```

A skip is logged at `[plugin]` level — visible but not fatal. The plugin still
initializes; its runtime `PluginDB` interface is already backend-agnostic. The
only thing it misses is its schema. If the tables don't exist, the plugin's
runtime queries will fail with clear "table not found" errors, which is better
than crashing the worker at startup.

### A.3 Update callers

`cmd/cleat-worker/main.go` already passes `plugin.Dialect(factory.Dialect())` to
`RunMigrations` (Stream D). No change needed.

**Stream A total: ~1 day**

---

## Stream B: MySQL Translations

**Goal:** Add `UpMySQL` to every migration in the 17 non-pgvector plugins.

**Files:** Each plugin's `migrations.go` (`plugins/*/migrations.go`)

### Translation rules (same as core Stream B)

| PostgreSQL | MySQL |
|-----------|-------|
| `UUID` | `CHAR(36)` |
| `JSONB` | `JSON` |
| `TIMESTAMPTZ` | `TIMESTAMP(6)` |
| `BYTEA` | `LONGBLOB` |
| `BOOLEAN` | `TINYINT(1)` |
| `TEXT[]` | `JSON` |
| `BIGSERIAL` | `BIGINT AUTO_INCREMENT` |
| `DOUBLE PRECISION` | `DOUBLE` |
| `gen_random_uuid()` | (remove — Go-side UUID generation) |
| `now()` | `NOW(6)` |
| `DEFAULT '{}'` (JSON) | `DEFAULT ('{}')` |
| `CREATE EXTENSION` | (remove) |

### Plugin-by-plugin breakdown

Each plugin's `Migrations()` function gains `UpMySQL` strings. Example:

```go
// Before
func (p *Plugin) Migrations() []plugin.Migration {
    return []plugin.Migration{{
        Version: 1,
        Up: `CREATE TABLE IF NOT EXISTS kv_store (
            key TEXT PRIMARY KEY,
            value JSONB NOT NULL DEFAULT '{}',
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )`,
    }}
}

// After
func (p *Plugin) Migrations() []plugin.Migration {
    return []plugin.Migration{{
        Version: 1,
        Up: `CREATE TABLE IF NOT EXISTS kv_store (
            key TEXT PRIMARY KEY,
            value JSONB NOT NULL DEFAULT '{}',
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )`,
        UpMySQL: `CREATE TABLE IF NOT EXISTS kv_store (
            kv_key VARCHAR(255) PRIMARY KEY,
            value JSON NOT NULL DEFAULT ('{}'),
            created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
            updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
        )`,
    }}
}
```

Note: `key` is a reserved word in MySQL — rename column to `kv_key` in the
MySQL variant (or backtick-quote it). Same issue may arise for other reserved
words across the 28 tables.

### Plugins to translate (17)

All plugins implementing `HasMigrations` except `pgvector`:

auditlog, blobstore, datadogexport, eventstore, eventtriggers, featureflags,
jobqueue, kafkaconnect, kvstore, notifications, oauthprovider, pagerdutyalert,
ratelimiter, scheduledbackup, scheduler, slacknotify, webhookingest

**Stream B total: ~2 days**

---

## Stream C: MSSQL Translations

**Goal:** Add `UpMSSQL` to every migration in the 17 non-pgvector plugins.

**Files:** Same `plugins/*/migrations.go` files as Stream B.

### Translation rules (same as core Stream C)

| PostgreSQL | SQL Server |
|-----------|------------|
| `UUID` | `UNIQUEIDENTIFIER` |
| `JSONB` | `NVARCHAR(MAX)` |
| `TIMESTAMPTZ` | `DATETIMEOFFSET` |
| `BYTEA` | `VARBINARY(MAX)` |
| `BOOLEAN` | `BIT` |
| `TEXT[]` | `NVARCHAR(MAX)` |
| `BIGSERIAL` | `BIGINT IDENTITY(1,1)` |
| `DOUBLE PRECISION` | `FLOAT(53)` |
| `gen_random_uuid()` | `NEWID()` |
| `now()` | `SYSUTCDATETIME()` |
| `CREATE TABLE IF NOT EXISTS` | `IF NOT EXISTS (...) CREATE TABLE` |
| `ON CONFLICT DO NOTHING` | `MERGE` or `IF NOT EXISTS` pattern |
| `CREATE EXTENSION` | (remove) |

### PK column width constraint

As fixed in Stream D (ec9591e), columns in PRIMARY KEY or UNIQUE constraints
must not use `NVARCHAR(MAX)`. Use `NVARCHAR(64)` (UUIDs), `NVARCHAR(255)`
(names), or `NVARCHAR(200)` (composite PKs to stay under 900 bytes).

### Example (same kvstore plugin)

```go
UpMSSQL: `IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'kv_store')
    CREATE TABLE kv_store (
        kv_key NVARCHAR(255) NOT NULL PRIMARY KEY,
        value NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
        created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
        updated_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
    )`,
```

### Reserved words

SQL Server reserved words (e.g., `key`, `status`, `name`) that appear as column
names must be bracketed: `[key]`, `[status]`, `[name]`.

**Stream C total: ~2 days**

---

## Stream D: Testing and CI

**Goal:** Prove the 17 plugins create tables and function correctly against all
three backends.

### D.1 Plugin table creation smoke test

Add a test that, for each registered backend, runs `RunMigrations` for every
plugin implementing `HasMigrations`, then verifies each expected table exists:

```go
func TestPluginMigrations_AllDialects(t *testing.T) {
    for _, backend := range registeredBackends {
        t.Run(backend.Name(), func(t *testing.T) {
            store, teardown := backend.Setup(t)
            defer teardown()
            // Run plugin migrations, verify tables present
        })
    }
}
```

This lives in `internal/host/` or `internal/plugin/` and reuses the
`StoreBackend` registration from Stream A.

### D.2 Run existing plugin behavioral tests against MySQL/MSSQL

Where a plugin has behavioral tests that use a real database, guard them with
the backend registration pattern so they can run against any available backend.
Most plugin tests currently hardcode PostgreSQL; this is a moderate refactor
but only needed for plugins where the test actually exercises DB queries.

### D.3 CI matrix

Add a plugin-migration job to `.github/workflows/multi-db-ci.yml`:

```yaml
test-plugin-migrations:
  strategy:
    matrix:
      db: [postgres, mysql, mssql]
  steps:
    - run: go test -run TestPluginMigrations ./internal/plugin/...
```

**Stream D total: ~2 days**

---

## Stream E: Documentation

### E.1 Plugin compatibility matrix

Add a table to `docs/database-backends.md` (created in Stream G):

| Plugin | PostgreSQL | MySQL | MSSQL | Notes |
|--------|-----------|-------|-------|-------|
| auditlog | Yes | Yes | Yes | |
| blobstore | Yes | Yes | Yes | |
| ... | | | | |
| pgvector | Yes | No | No | Requires pgvector extension |

### E.2 Plugin developer guide

Add a section to `CONTRIBUTING.md` or a new `docs/plugin-development.md`:

- How to write dialect-specific migrations
- Translation reference tables (PG → MySQL, PG → MSSQL)
- Reserved words to watch for per backend
- Testing against multiple databases locally

**Stream E total: ~0.5 day**

---

## Effort Summary

| Stream | Description | Can Start | Duration |
|--------|-------------|-----------|----------|
| A | Migration API — dialect fields + runner dispatch | Immediately | 1 day |
| B | MySQL — translate 17 plugin migrations | After A | 2 days |
| C | MSSQL — translate 17 plugin migrations | After A | 2 days |
| D | Testing and CI | After B+C | 2 days |
| E | Documentation | Throughout | 0.5 day |
| F | Runtime query portability | After B+C+D | 5.25 days |

### Parallel schedule

```
Day 1:    [A: API change ...................................]
Day 2-3:  [B: MySQL translations ...........................]
          [C: MSSQL translations ...........................]
Day 4-5:  [D: Testing and CI ..............................]
          [E: Documentation ...............]
Day 6-8:  [F.0: Test rot fix ....][F.1: Rebind+Query ..][F.2: Dialect ..]
          [F.3: Per-plugin query porting ....................................]
          [F.4: Driver fixes ...............................................]
Day 9-10: [F.5: CI behavioral matrix ......................................]
```

**Total: ~12.75 days** for one developer. With parallel AI assistance, 5-7 days.

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| MySQL reserved word clashes (`key`, `status`) | High | Low | Rename columns or backtick-quote in MySQL variant only |
| MSSQL NVARCHAR(MAX) in PKs (same bug Stream D fixed) | Medium | Medium | Enforce Stream D rules in translation; CI catches DDL failures |
| Plugin that silently depends on PostgreSQL-specific query syntax at runtime (not just DDL) | Medium | High | Addressed by Stream F — Rebind eliminates placeholder issues; Query struct handles structural differences; CI runs behavioral tests against all three backends |
| Column type mismatch between migration and runtime queries (e.g., Go code expects `JSONB` but gets `JSON`) | Low | Medium | PluginDB interface already abstracts DB access; most plugins use `json.RawMessage` in Go, not SQL JSON operators |
| Plugin author forgets to add MySQL/MSSQL SQL for new migration | Medium | Low | Skip-with-warning behavior means worker still starts; CI catches at PR time |
| `Rebind` corrupts `$` in non-placeholder contexts (JSON `'$schema'`) | Low | Medium | Regex requires `$` followed by digit; CI behavioral matrix catches corruption |
