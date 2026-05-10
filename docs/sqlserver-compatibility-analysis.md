# SQL Server / Azure SQL Database Compatibility Analysis

May 2026 — extending the multi-database feasibility analysis to cover Microsoft
SQL Server (including Azure SQL Database) as a third major database family
alongside PostgreSQL and MySQL.

---

## 1. The Short Answer

The `WorkflowStore` interface is sufficient for SQL Server. In several important
ways, SQL Server is a **better architectural fit than MySQL**: it has Row-Level
Security, schemas, native percentile/hash functions, and an `OUTPUT` clause that
replaces `RETURNING`. The interface changes needed are the same ones already
identified in the MySQL analysis — no SQL Server-specific interface modifications
are required. A production-quality implementation would take approximately
22-23 days of engineering effort (vs ~27 for MySQL).

---

## 2. The Claim Pattern: SQL Server's Advantage

The core of the worker dispatch loop is atomically claiming workflow instances.
Each database has different tooling for this:

| Feature | PostgreSQL | MySQL 8.0+ | SQL Server 2017+ |
|---------|-----------|------------|------------------|
| Skip locked rows | `SKIP LOCKED` | `SKIP LOCKED` | `READPAST` hint (2017+) or `SKIP LOCKED` keyword (2022+) |
| Atomic claim-and-read | `RETURNING *` | Two-statement transaction | `OUTPUT INSERTED.*` |
| **Claim in one atomic statement?** | Yes (`SELECT FOR UPDATE`) | No (SELECT + UPDATE) | **Yes** (`UPDATE TOP N ... OUTPUT INSERTED.*`) |

SQL Server can claim and return data in a single `UPDATE ... OUTPUT` statement:

```sql
UPDATE TOP(@limit) w
SET assigned_to = @workerID,
    status = 'running',
    heartbeat_at = DATEADD(SECOND, @timeout, SYSUTCDATETIME())
OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
       INSERTED.input, INSERTED.min_version, INSERTED.tenant_id,
       INSERTED.next_wake_at, INSERTED.created_at
FROM workflow_instances w WITH (READPAST, UPDLOCK, ROWLOCK)
WHERE w.status = 'ready'
  AND w.next_wake_at <= SYSUTCDATETIME()
  AND w.tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
ORDER BY w.created_at;
```

This is architecturally cleaner than the PostgreSQL pattern. One statement.
No race between SELECT and UPDATE. The interface doesn't care — it just returns
`[]*WorkflowInstance`.

---

## 3. Row-Level Security: SQL Server Matches PostgreSQL

This is the biggest gap between PostgreSQL and MySQL, and where SQL Server
proves its enterprise pedigree:

| Database | RLS | Session Context | Schema Isolation |
|----------|-----|-----------------|------------------|
| PostgreSQL | `CREATE POLICY` + `USING` clause | `set_config()` / `current_setting()` | `search_path` DSN parameter |
| MySQL 8.0+ | **Not supported** | N/A | Separate databases per tenant |
| SQL Server 2016+ | `CREATE SECURITY POLICY` + inline TVF | `sp_set_session_context` / `SESSION_CONTEXT()` | `EXECUTE AS USER` or schema isolation |

The migration is a near-direct translation:

```sql
-- PostgreSQL (from migration 002)
CREATE POLICY tenant_isolation ON workflow_instances
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id')::uuid);

-- SQL Server equivalent
CREATE FUNCTION dbo.fn_tenant_filter()
RETURNS TABLE
WITH SCHEMABINDING
AS RETURN
  SELECT 1 AS fn_tenant_filter_result
  WHERE tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER);

CREATE SECURITY POLICY dbo.TenantFilter
  ADD FILTER PREDICATE dbo.fn_tenant_filter() ON dbo.workflow_instances,
  ADD BLOCK PREDICATE dbo.fn_tenant_filter() ON dbo.workflow_instances;
```

The `WorkflowStore` interface never exposes tenant isolation to callers — it's
purely a factory/connection-setup concern. Each store backend injects its own
tenant context at connection open time.

**This is a decisive advantage over MySQL.** MySQL's lack of RLS means
application-level `WHERE tenant_id = ?` on every single query — a fundamentally
weaker security model where a single missing clause is a data leak. SQL Server
RLS, like PostgreSQL RLS, enforces isolation at the database engine level.

---

## 4. Feature Compatibility Matrix

### 4.1 Functions with Direct Equivalents

| PostgreSQL | Locations | SQL Server Equivalent | Difficulty |
|-----------|-----------|----------------------|------------|
| `gen_random_uuid()` | 6+ | `NEWID()` or Go-side | Trivial |
| `digest($1, 'sha256')` | 3 | `HASHBYTES('SHA2_256', $1)` | Trivial |
| `percentile_cont()` | 8 | `PERCENTILE_CONT()` (SQL 2022+) or Go-side | Low |
| `now()` | 15+ | `SYSUTCDATETIME()` or `GETUTCDATE()` | Trivial |
| `::type` casts | 15+ | `CAST($1 AS type)` | Trivial |
| `EXTRACT(EPOCH FROM ...)` | 1 | `DATEDIFF_BIG(SECOND, '1970-01-01', ...)` | Trivial |
| `ILIKE` | 3 | `LOWER(col) LIKE LOWER(val)` | Low |
| `RETURNING` | 12+ | `OUTPUT INSERTED.*` / `OUTPUT DELETED.*` | Low |

### 4.2 Functions Requiring Workarounds

| PostgreSQL | Locations | SQL Server Approach | Difficulty |
|-----------|-----------|--------------------|------------|
| `ON CONFLICT DO NOTHING` | 10+ | `MERGE` statement or `IF NOT EXISTS (SELECT ...) INSERT` | Low-medium |
| `ON CONFLICT DO UPDATE SET` | Few | `MERGE ... WHEN MATCHED THEN UPDATE ... WHEN NOT MATCHED THEN INSERT` | Medium |
| `pq.Array()` / `ANY($1)` | 2 | Dynamic `IN (...)` clause building | Medium |
| JSONB columns | All tables | `NVARCHAR(MAX)` with `JSON_VALUE`/`JSON_QUERY` | Medium |
| PL/pgSQL functions (migration 009) | 4 funcs | T-SQL stored procedures | Medium |
| `search_path` schema routing | DSN setup | `EXECUTE AS USER` or schema prefix per tenant | Medium |

### 4.3 Data Type Translation

| PostgreSQL | SQL Server | Go Type |
|-----------|------------|---------|
| `UUID` | `UNIQUEIDENTIFIER` | `string` |
| `BIGSERIAL` | `BIGINT IDENTITY(1,1)` | `int64` |
| `TEXT` | `NVARCHAR(MAX)` | `string` |
| `JSONB` | `NVARCHAR(MAX)` (+ `ISJSON` check constraint) | `json.RawMessage` |
| `BYTEA` | `VARBINARY(MAX)` | `[]byte` |
| `TIMESTAMPTZ` | `DATETIMEOFFSET` | `time.Time` |
| `DOUBLE PRECISION` | `FLOAT(53)` | `float64` |
| `BOOLEAN` | `BIT` | `bool` |
| `TEXT[]` | Separate table or JSON column | `[]string` (in Go, not SQL) |

---

## 5. Interface Sufficiency Assessment

The `WorkflowStore` interface has 63 methods. The method signatures use only Go
types (`string`, `int`, `time.Time`, `json.RawMessage`, `[]byte`, custom
structs). No SQL dialect leaks into the interface.

**Methods needing zero change for SQL Server: 58 of 63**

All the standard CRUD, lifecycle, and query methods translate cleanly.

### Known issues

1. **`TraceWorkflow` returns `sql.Result`** (line 262 of `db.go`): Already
   identified as a leak. Fixing to `error` applies to all non-PostgreSQL
   backends equally.

2. **Error taxonomy gap**: If callers need to distinguish "deadlock → retry"
   from "constraint violation → fail," the interface needs standard error
   sentinels. This affects ALL multi-DB implementations, not just SQL Server.
   SQL Server error codes: deadlock = 1205, unique violation = 2627.

3. **`ContinueAsNew` transaction model**: The method signature says "atomically
   creates a new workflow run AND completes the current one in a single database
   transaction." SQL Server supports `BEGIN TRANSACTION ... COMMIT` with the
   same semantics. No interface change needed.

4. **Pagination**: `ListWorkflows` uses `Offset/Limit` in `WorkflowFilter`.
   SQL Server uses `OFFSET ... FETCH NEXT N ROWS ONLY` (different syntax, same
   semantics). Purely an implementation detail.

### What might need to be ADDED to the interface

Nothing specific to SQL Server. The existing plan's recommendations (add
`CleanupIdempotencyKeys()`, `ResolveTenantFromAPIKey()`, `DeployPlugin()`) are
needed for any multi-DB implementation.

---

## 6. SQL Server-Specific Implementation Concerns

### 6.1 Connection Management

SQL Server uses a fundamentally different connection model than PostgreSQL:

- **Connection strings** (`Server=...;Database=...;Authentication=...`)
  instead of DSNs (`postgres://user:pass@host/db?options...`)
- **Authentication modes**: SQL auth, Azure Active Directory (token-based,
  managed identity), Windows Integrated. The factory needs to handle all three.
- **Pooling**: The `microsoft/go-mssqldb` driver has different pooling semantics
  than `lib/pq`. Pool fragmentation can occur if many unique connection strings
  are used (one per tenant). Azure SQL throttles logins — connection pooling
  is critical.

Contained entirely in the `StoreFactory` implementation.

### 6.2 Transaction Isolation and Locking

SQL Server's locking model uses hints rather than clauses:

```sql
-- PostgreSQL
SELECT ... FOR UPDATE SKIP LOCKED

-- SQL Server
SELECT ... WITH (UPDLOCK, READPAST, ROWLOCK)
```

`READPAST` is equivalent to `SKIP LOCKED`. `UPDLOCK` takes update locks
(preventing deadlocks in the claim pattern). `ROWLOCK` prevents lock escalation
to page/table level.

Contained entirely in the claim method implementations.

### 6.3 Identifier Quoting

`pq.QuoteIdentifier()` uses double quotes (`"schema"."table"`). SQL Server uses
square brackets (`[schema].[table]`) or double quotes with `SET QUOTED_IDENTIFIER ON`.
The existing plan (Leak 4) already identifies quoting as a concern to contain
inside each store implementation.

### 6.4 Azure SQL Database Specifics

Azure SQL Database has minor differences from on-prem SQL Server:

- No `USE` statement (connection specifies database)
- No SQL Agent (cron-like scheduling not used by cleat)
- Built-in automatic tuning, index recommendations
- Mandatory TLS 1.2
- Connection resiliency: transient fault handling with retry logic is
  essential (Azure SQL Gateway can disconnect idle connections)

These affect the factory (connection retry logic) but not the interface.

### 6.5 Migration Files

11 migration files need T-SQL equivalents. Key syntax differences:

- `CREATE OR REPLACE FUNCTION` → `CREATE OR ALTER PROCEDURE`
- `RETURNS TRIGGER` → `RETURNS TRIGGER` (similar but T-SQL)
- `BIGSERIAL` → `BIGINT IDENTITY(1,1)`
- `CREATE INDEX ... INCLUDE (...)` → same syntax (SQL Server originated this!)
- `EXECUTE format(...)` → `EXEC sp_executesql` with parameterized SQL
- `DECLARE ... BEGIN ... END` → `BEGIN ... END` (T-SQL block syntax)

Mechanical translation, similar effort to MySQL migration translation.

---

## 7. Effort Comparison: MySQL vs SQL Server

### Prototype (validates interface, throwaway code)

| Phase | MySQL | SQL Server | Notes |
|-------|-------|------------|-------|
| De-leak interface | 1.5 days | Already done | One-time cost |
| Schema migration translation | 1 day | 1.5 days | T-SQL is more verbose |
| Core claim/execute | 1 day | 1 day | UPDATE...OUTPUT is single-statement (simpler than MySQL) |
| Event history | 1 day | 1 day | MERGE handles upsert |
| Lifecycle (complete/fail/continue) | 1 day | 1 day | Same pattern |
| Signals, promises, updates | 1 day | 1 day | CRUD, no RETURNING dependency |
| List/search | 0.5 days | 0.5 days | Same dynamic query building |
| Memory stats | 0.5 days | 0.5 days | SQL Server has native PERCENTILE_CONT |
| Tenant isolation | Stubbed | 1 day | SQL Server RLS maps cleanly |
| Stored procedures | Stubbed | 1 day | T-SQL rewrite |
| Integration testing | 2 days | 2 days | Same test suite |
| Driver/pooling setup | 0.5 days | 1 day | Different connection model |
| **Prototype total** | **~8 days** | **~10-11 days** | |

### Production-quality implementation

| Phase | MySQL | SQL Server | Notes |
|-------|-------|------------|-------|
| Tenant isolation (proper) | 5 days (app-level, no RLS) | 2 days (RLS policies) | Major TCO advantage for SQL Server |
| Performance tuning | 3 days | 3 days | Same effort |
| Error code mapping | 2 days | 2 days | Same effort |
| Production hardening | 2 days | 2 days | Same effort |
| Migration DSL | 5 days | Already built | Shared infrastructure |
| Documentation | 2 days | 2 days | Same effort |
| **Production total** | **~27 days** | **~22-23 days** | SQL Server is faster |

SQL Server requires less effort at production quality because:
- RLS exists (saves 3 days vs building app-level tenant filtering)
- OUTPUT clause enables single-statement claim (fewer edge cases)
- Native HASHBYTES and PERCENTILE_CONT (less Go-side workaround code)

---

## 8. Strategic Assessment

### SQL Server vs MySQL as the Second Backend

| Factor | MySQL | SQL Server |
|--------|-------|------------|
| Architectural fit | Moderate (no RLS, no RETURNING, no percentiles) | Strong (RLS, OUTPUT, PERCENTILE_CONT, HASHBYTES) |
| Claim pattern | Two-statement transaction (SELECT + UPDATE) | Single-statement (UPDATE...OUTPUT) |
| Tenant isolation | Application-level WHERE clause (weaker security) | Database-engine RLS (same level as PostgreSQL) |
| Enterprise adoption | Common in startups, web companies | Common in large enterprises, .NET/Azure shops |
| Deployment simplicity | Simple (single binary + MySQL) | Moderate (SQL Server licensing, or Azure subscription) |
| Open source vs commercial | Fully open source (MySQL Community) | SQL Server Developer is free; production requires licensing |
| Market overlap with PostgreSQL | Significant (both open-source RDBMS) | Different market segment (Microsoft ecosystem) |

### Recommendation

**If multi-DB is greenlit, target SQL Server before MySQL.**

The reasoning:
1. SQL Server opens a truly different customer segment (Microsoft/Azure shops)
   than PostgreSQL, where MySQL substantially overlaps with PostgreSQL.
2. SQL Server's RLS, schemas, and OUTPUT clause mean the implementation is
   architecturally cleaner — less workaround code, fewer edge cases.
3. The interface de-leaking and migration DSL are shared costs. Going from 1→2
   databases is the expensive jump; 2→3 is incremental.
4. MySQL remains a valid third backend — and by the time it's needed, the
   migration DSL, error taxonomy, shared test suite, and abstraction patterns
   are all proven.

### Concrete Roadmap Impact

This analysis does not change the recommendation: multi-DB remains an **18-24
month roadmap item**, gated on demonstrated demand. But when the time comes,
SQL Server should be the first target, not MySQL.

**SQLite remains the exception worth considering sooner** (for embedded/edge
and zero-setup developer experience), independent of the enterprise multi-DB
decision.
