# Multi-Database Feasibility for Cleat

May 2026 — analysis of how deeply cleat is coupled to PostgreSQL, what it would
take to support another database, and whether the cost is worth paying now.

---

## 1. The Short Answer

Technically feasible but deeply expensive. Cleat is coupled to PostgreSQL at
~50 distinct query locations across ~3,000 lines of store code. Supporting
another database is a multi-month engineering effort, not a configuration
change. The architecture makes it possible — but the timing makes it premature.

---

## 2. Where the Coupling Lives

There is a `WorkflowStore` interface (60+ methods in
`internal/host/db.go:117-375`) — a real abstraction boundary. But there is only
one implementation: `PostgresStore`. The interface is clean in principle but
leaky in practice (callers type-assert to concrete PostgreSQL types, and the
`ShardedStore` wraps `*PostgresStore` directly rather than the interface).

The coupling breaks down into four tiers:

### Tier 1 — Critical Architectural Dependency (No ANSI Equivalent)

| Feature | Where | What It Does |
|---------|-------|--------------|
| `FOR UPDATE SKIP LOCKED` | `db.go:475,542,1876` | Worker claim dispatch — the core poll loop. Marks rows as claimed in one atomic operation without blocking other workers. |
| `RETURNING` clause | 12+ query locations | Atomic claim-and-read — claim a workflow row, get its data back in one round-trip. Essential for the claim-execute pattern. |
| Row-Level Security + `set_config`/`current_setting` | `db.go:418,675`, migration 002 | Multi-tenant isolation enforced at the database level. Every query runs in a session scoped to a tenant UUID. |
| PL/pgSQL stored procedures | migration 009, 4 functions | Tenant provisioning — creating login roles, granting plugin access, using `EXECUTE format(...)` dynamic SQL. |

### Tier 2 — Pervasive but Replaceable PostgreSQL Syntax

| Feature | Count | Replacement Difficulty |
|---------|-------|----------------------|
| `ON CONFLICT ... DO NOTHING` (upsert) | 10+ locations | Medium — application-level idempotency check before INSERT, or `MERGE` (SQL:2023, varying support) |
| `::type` casts | 15+ locations | Low — move to Go-side value conversion, or use `CAST()` syntax |
| `gen_random_uuid()` | 6+ locations | Trivial — generate UUIDs in Go before the query |
| `digest($1, 'sha256')` (pgcrypto) | 3 locations | Medium — move hashing to Go, but idempotency key lookups change |
| `percentile_cont()` | 8 locations | Low — compute percentiles in Go (memory stats are infrequent) |
| `ILIKE` | 3 locations | Low — application-side case folding or dialect-specific replacement |
| `EXTRACT(EPOCH FROM ...)` | 1 location | Trivial — compute epoch in Go |
| `now()` in SQL | 15+ locations | Low — pass parameterized timestamps from Go |
| `pq.Array()` for `ANY($1)` | 2 locations | Medium — task queue filtering; replace with `IN` clause or dynamic SQL |

### Tier 3 — Infrastructure Assumptions

| Area | PostgreSQL-Specific Assumption |
|------|-------------------------------|
| Schema isolation | `search_path` DSN parameter — each tenant/workload gets its own PostgreSQL schema. Other databases have different multi-tenancy models. |
| Connection pooling | `lib/pq` driver assumptions (`SetMaxOpenConns`, DSN format `postgres://...`). Cross-database pooling requires a driver abstraction layer. |
| Sharding | `ShardedStore` wraps `*PostgresStore` by concrete type. `CREATE SCHEMA IF NOT EXISTS` and `search_path` injection are PostgreSQL-specific. |
| JSONB columns | Every table uses `JSONB` for payloads. Cleat already base64-encodes binary data before storage, so JSONB is mostly a text container — not deeply dependent on jsonb query operators. |
| Migrations | All 11 migration files are PostgreSQL SQL. A new database needs a parallel migration tree or a migration DSL. |
| Test infrastructure | Test schema setup uses `sql.Open("postgres", ...)` directly. Requires `pgcrypto` extension. |

### Tier 4 — What Does NOT Depend on PostgreSQL

- The WASM runtime (wazero) — zero database dependency
- The transformer pipeline (Go AST → WASM) — operates on files, not databases
- The SDK surface (HostCalls interface) — callers never see the database
- Plugin host function implementations — they receive JSON, return JSON
- The web UI (Svelte SPA) — talks to the REST API, not the database
- The Helm chart and deployment model

---

## 3. What a Multi-DB Implementation Would Require

### Step 1: De-leak the WorkflowStore Interface

The interface mostly exists but callers type-assert to PostgreSQL types and the
sharding layer uses concrete `*PostgresStore`. Cleanup: ~1-2 weeks.

### Step 2: Rewrite the Store for a New Database (~2,850 Lines)

The critical path is replacing `SKIP LOCKED` + `RETURNING` with an equivalent
atomic claim pattern. Options by target:

| Database | SKIP LOCKED | RETURNING | Viable? |
|----------|-------------|-----------|---------|
| MySQL 8.0+ | Supported | Not supported — use `LAST_INSERT_ID()` or follow-up SELECT in transaction | Yes, with claim pattern changes |
| SQLite | Not supported — requires `BEGIN IMMEDIATE` + poll loop | Supported | Yes, with higher contention at scale |
| CockroachDB | Supported | Supported | Yes, but no schema-per-tenant model |
| MariaDB 10.6+ | Supported | Supported (10.5+) | Yes |

MySQL is the most plausible second target — it has `SKIP LOCKED` in 8.0+.
SQLite is interesting for embedded/edge deployments and zero-setup developer
experience, though the claim model is fundamentally different.

### Step 3: Replace or Abstract Tenant Isolation

This is the hardest part. PostgreSQL RLS + `search_path` schema routing is a
genuine architectural advantage:

- **MySQL**: Separate databases per tenant (requires connection-per-tenant)
- **SQLite**: Separate files per tenant (natural for embedded, awkward for servers)
- **CockroachDB**: Application-level `WHERE tenant_id = $1` on every query (no RLS)

This is not a mechanical translation — it's a different security model with
different threat boundaries.

### Step 4: Write Parallel Migrations

11 migration files, ~500 lines of SQL, rewritten for each target dialect.

### Step 5: Abstract Connection Pool Management

A driver interface wrapping `database/sql` connections with database-specific
initialization (schema setup, tenant scoping, connection validation).

### Effort Estimate

- **First additional database (MySQL recommended)**: 2-3 months for a solo
  engineer who already knows the codebase
- **Each subsequent database**: 4-6 weeks
- **Ongoing maintenance tax**: Every new `WorkflowStore` method, every new
  migration, every query optimization now ships in N dialects

This is roughly 25-40% of the effort that went into building the engine itself.

---

## 4. Cost/Benefit Analysis

### Arguments For Multi-DB

- **Expands the addressable market.** Some teams are MySQL shops, some use
  CockroachDB for multi-region, some want SQLite for embedded/edge deployments.
  PostgreSQL-only eliminates them entirely.
- **Enterprise sales enabler.** "We're a MySQL shop" is a hard no without
  multi-DB. With it, you can say "we support your stack."
- **Reduces "yet another infrastructure dependency" objections.** If a team
  already runs MySQL, cleat on MySQL is still "zero new services" — but only
  if MySQL is supported.
- **SQLite opens embedded/edge use cases.** Single-binary deployments (Electron
  apps, edge devices, CI pipelines) are a different market from server-side
  workflows, but potentially large.
- **Architectural validation.** Building a second store implementation proves
  the `WorkflowStore` interface is real, not an accidental shape of the
  PostgreSQL implementation. This makes the codebase healthier and catches
  abstraction leaks.

### Arguments Against Multi-DB (Right Now)

- **Zero users.** Cleat has no production users on PostgreSQL. Optimizing for
  database portability before anyone uses it on any database is premature
  abstraction at the architectural level.
- **The PostgreSQL-only pitch IS the differentiation.** "Uses your existing
  PostgreSQL" is cleat's core wedge. It's what makes "zero new stateful
  services" true. If someone doesn't run PostgreSQL, they're not the target
  user — at least not yet. Spreading to other databases dilutes the pitch
  before it's proven.
- **2-3 months of engineering for a solo developer** is 2-3 months not spent
  on the adoption work that actually matters (demo video, blog posts, design
  partner outreach, dogfooding).
- **Ongoing maintenance cost.** Every new feature ships in N dialects. Every
  migration runs against N databases. Every query optimization is
  database-specific. This is a permanent tax on development velocity.
- **The SKIP LOCKED gap is real for some targets.** SQLite doesn't have it.
  That means a fundamentally different claim model that could re-introduce
  exactly-once bugs already fixed for PostgreSQL. You'd be maintaining two
  correctness models.
- **Competitors don't support multiple databases either.** Temporal Server
  supports multiple databases for its own storage, but the programming model
  is database-agnostic (users don't pick a database). DBOS is PostgreSQL-only.
  Multi-DB is not table stakes in this category.

---

## 5. Recommendation

**Put multi-database support on the 18-24 month roadmap, not the 6-month
roadmap.**

The reasoning:

1. The PostgreSQL-only wedge is cleat's strongest differentiator right now.
   "You already run PostgreSQL — cleat just connects to it" is a cleaner story
   than "cleat works with many databases."

2. The cost (2-3 months of solo engineering) is too high relative to the
   urgent priority: get one production user. That time should go to adoption
   work.

3. Multi-DB becomes worth doing when one of these gating conditions is met:
   - **10+ paying cloud customers** and a significant prospect says "we'd buy
     but we're MySQL-only"
   - **A design partner** is willing to fund MySQL/SQLite support as a paid
     engagement
   - **Community contributors** express interest in building a second store
     backend (the `WorkflowStore` interface makes this feasible for an external
     contributor)

4. **One exception worth considering sooner: SQLite.** If embedded/edge
   deployment becomes a strategic priority, or if you want a zero-setup
   "cleat local" developer experience that doesn't require Docker, SQLite
   support could replace the "install PostgreSQL in Docker" step in the
   getting-started tutorial. This is a few weeks of work and would improve the
   first-run experience. But even this should wait until there is at least one
   production PostgreSQL user.

### Concrete Roadmap Entry

> **Multi-database support (2027+)**: The `WorkflowStore` interface provides a
> clean abstraction boundary. MySQL (which supports `SKIP LOCKED` in 8.0+) is
> the most plausible second target. SQLite support would enable embedded/edge
> deployments and simplify the getting-started experience. Gated on:
> demonstrated demand from production users or a funded design partner
> engagement. Not planned for the initial cloud launch.

The architecture makes multi-DB possible. The timing makes it premature. Don't
build the second database backend before the first one has users.
