# Database Backends

Cleat's durable workflow engine runs on three production-quality relational
database backends. All three provide the full `WorkflowStore` interface
(72 methods) behind a common `StoreFactory` abstraction — application code
never needs to know which database is in use. Choose the backend that fits
your existing infrastructure.

---

## 1. Overview

The `WorkflowStore` interface (`internal/host/db.go`) defines every operation
the engine needs: workflow lifecycle, event history, signals, scheduling,
promises, concurrency control, version management, memory statistics, and
tenant isolation. Each backend has a complete implementation:

- **PostgreSQLStore** (`internal/host/db.go`) — the original implementation
- **MySQLStore** (`internal/host/mysql_store.go`) — MySQL 8.0+, MariaDB 10.6+
- **MSSQLStore** (`internal/host/mssql_store.go`) — SQL Server 2017+, Azure SQL

Each backend also has a `StoreFactory` that encapsulates connection management,
schema setup, and tenant isolation:

- `PostgresStoreFactory` — takes an open `*sql.DB` and optional schema name
- `MySQLStoreFactory` — manages per-tenant databases with dedicated connection pools
- `MSSQLStoreFactory` — wraps connections with RLS session context

```go
type StoreFactory interface {
    OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error)
    DriverName() string       // "postgres", "mysql", or "mssql"
    Dialect() Dialect         // DialectPostgres, DialectMySQL, or DialectMSSQL
}
```

---

## 2. Supported Versions

| Backend | Minimum Version | Recommended Version | Notes |
|---------|----------------|---------------------|-------|
| PostgreSQL | 14 | 16 | RLS, `SKIP LOCKED`, `gen_random_uuid()`, and `JSONB` all available since 9.5+. 14+ ensures pgcrypto support. |
| MySQL | 8.0 | 8.4 | `SKIP LOCKED` requires 8.0+. `NOW(6)` for microsecond precision. |
| MariaDB | 10.6 | 11.x | Tested alongside MySQL 8.4. Supports `SKIP LOCKED`. Does not support RLS. |
| SQL Server | 2017 | 2022 | `STRING_SPLIT` (used for task queue filtering) requires compatibility level 130 (2016+). Azure SQL Database fully supported. |

---

## 3. Feature Comparison

| Capability | PostgreSQL | MySQL 8.0+ / MariaDB | SQL Server 2017+ |
|------------|-----------|----------------------|-------------------|
| **Tenant isolation** | RLS via `set_config()` + `CREATE POLICY` | Separate database per tenant (application-level `WHERE tenant_id = ?`) | RLS via `sp_set_session_context()` + `CREATE SECURITY POLICY` |
| **Atomic claim read** | `UPDATE ... RETURNING *` (single statement) | SELECT + UPDATE + SELECT (three statements in a transaction) | `UPDATE ... OUTPUT INSERTED.*` (single statement) |
| **Skip locked rows** | `FOR UPDATE SKIP LOCKED` | `FOR UPDATE SKIP LOCKED` | `WITH (READPAST, UPDLOCK, ROWLOCK)` |
| **JSON querying** | `JSONB` with full operators (`->`, `->>`, `@>`, `?`) | `JSON` with `JSON_EXTRACT`, `JSON_UNQUOTE` | `NVARCHAR(MAX)` with `JSON_VALUE`, `JSON_QUERY` |
| **Full-text search** | `tsvector` / `tsquery` | `MATCH ... AGAINST` (FULLTEXT index) | `CONTAINS` / `FREETEXT` (full-text indexes) |
| **Native SHA-256** | `digest(x, 'sha256')` via pgcrypto | Not available (computed in Go) | `HASHBYTES('SHA2_256', x)` |
| **Native percentiles** | `percentile_cont()` | Not available (computed in Go) | `PERCENTILE_CONT()` |
| **Partial indexes** | `CREATE INDEX ... WHERE ...` | Not supported | `CREATE INDEX ... WHERE ...` (filtered indexes) |
| **Upsert** | `ON CONFLICT DO UPDATE` / `DO NOTHING` | `ON DUPLICATE KEY UPDATE` / `INSERT IGNORE` | `MERGE` / `INSERT ... WHERE NOT EXISTS` |
| **Event idempotency** | `ON CONFLICT DO NOTHING` | `INSERT IGNORE` | `INSERT ... SELECT ... WHERE NOT EXISTS` |
| **Signal upsert** | `ON CONFLICT DO UPDATE` | `ON DUPLICATE KEY UPDATE` | `MERGE` |
| **Timestamp precision** | `TIMESTAMPTZ` (microsecond) | `TIMESTAMP(6)` (microsecond) | `DATETIMEOFFSET` (100 nanosecond) |
| **UUID generation** | `gen_random_uuid()` or Go-side | Go-side only | `NEWID()` or Go-side |
| **Full WorkflowStore** | 72/72 methods | 72/72 methods | 72/72 methods |
| **Migration files** | 10 | 10 | 10 |
| **CI-tested** | Yes (primary) | Yes (dedicated workflow) | Yes (dedicated workflow) |

---

## 4. Configuration Per Backend

### 4.1 PostgreSQL

**Connection string format (DSN):**

```
postgres://username:password@host:port/dbname?sslmode=disable&search_path=public
```

Examples:

```
postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable
postgres://cleat:cleat@postgres:5432/cleat?sslmode=disable
postgres://cleat:cleat@cleat-db.internal:5432/cleat?sslmode=require&search_path=cleat
```

**Environment variable (used by tests):**

```
CLEAT_TEST_POSTGRES=postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable
```

Fallback: `CLEAT_TEST_DB`, then `postgres://localhost:5432/cleat?sslmode=disable`.

**Go driver:**

```go
import _ "github.com/lib/pq"
```

**Factory setup:**

```go
// NewPostgresStoreFactory takes an already-open *sql.DB and a schema name.
// The schemaName defaults to "public" if empty.
factory := host.NewPostgresStoreFactory(db, "public")

// OpenStore creates a PostgresStore scoped to the given tenant.
// It creates the schema if needed (CREATE SCHEMA IF NOT EXISTS).
store, closer, err := factory.OpenStore(ctx, tenantID, "default")
defer closer.Close()
```

**Key connection pool parameters (set on `*sql.DB`):**

| Parameter | Recommended | Notes |
|-----------|-------------|-------|
| `SetMaxOpenConns` | 25 | Per connection pool; tune based on workload concurrency. |
| `SetMaxIdleConns` | 10 | Keep idle connections ready to avoid connection storms on the database. |
| `SetConnMaxLifetime` | 30 minutes | Prevent stale connections from accumulating. |
| `SetConnMaxIdleTime` | 5 minutes | Close idle connections during lulls. |

**Docker Compose:**

```yaml
postgres:
  image: postgres:16
  container_name: cleat-postgres
  environment:
    POSTGRES_USER: cleat
    POSTGRES_PASSWORD: cleat
    POSTGRES_DB: cleat
  ports:
    - "5432:5432"
  healthcheck:
    test: ["CMD-SHELL", "pg_isready -U cleat -d cleat"]
    interval: 3s
    timeout: 5s
    retries: 10
    start_period: 10s
  volumes:
    - pgdata:/var/lib/postgresql/data
```

---

### 4.2 MySQL / MariaDB

**Connection string format (Go MySQL driver DSN):**

```
username:password@tcp(host:port)/dbname?parseTime=true&multiStatements=true&charset=utf8mb4
```

Examples:

```
root:cleat@tcp(127.0.0.1:3306)/cleat?parseTime=true&multiStatements=true
cleat:cleat@tcp(mysql:3306)/cleat?parseTime=true&multiStatements=true&charset=utf8mb4
```

The `parseTime=true` parameter is required — the Go MySQL driver does not scan
`TIMESTAMP`/`DATETIME` columns into `time.Time` without it.

The `multiStatements=true` parameter is required for running migration scripts
that contain multiple SQL statements in a single `Exec`.

**Environment variable (used by tests):**

```
CLEAT_TEST_MYSQL=root:cleat@tcp(127.0.0.1:3306)/cleat?parseTime=true&multiStatements=true
```

CI uses `root:cleat@tcp(127.0.0.1:3306)/cleat` (without query parameters;
the factory adds what it needs). Tests are skipped if `CLEAT_TEST_MYSQL` is not
set.

**Go driver:**

```go
import _ "github.com/go-sql-driver/mysql"
```

**Factory setup:**

The MySQL factory uses a separate-database-per-tenant isolation model. It
requires a "master" connection (without a default database) for administrative
operations like `CREATE DATABASE`, plus a base DSN template that the factory
uses to build per-tenant connection strings.

```go
// Step 1: Open a master connection with no default database.
// The trailing slash before ? is important -- it means "no database."
masterDB, err := sql.Open("mysql", "root:cleat@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true")

// Step 2: Create the factory with the base DSN template (also no database).
factory := host.NewMySQLStoreFactory(masterDB, "root:cleat@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true")

// Step 3: Open a store scoped to a tenant. The factory creates the
// cleat_<tenant_id> database if it does not already exist and opens a
// dedicated connection pool for it.
store, closer, err := factory.OpenStore(ctx, tenantID, "default")
defer closer.Close()
```

The factory configures per-tenant connection pools with:

```go
tenantDB.SetMaxOpenConns(15)
tenantDB.SetMaxIdleConns(5)
tenantDB.SetConnMaxLifetime(5 * time.Minute)
```

**Key connection pool parameters:**

| Parameter | Recommended | Notes |
|-----------|-------------|-------|
| `SetMaxOpenConns` | 15 per tenant pool | MySQL connection overhead is higher than PostgreSQL; be conservative. |
| `SetMaxIdleConns` | 5 per tenant pool | Enough to avoid reconnect latency under light load. |
| `SetConnMaxLifetime` | 5 minutes | MySQL's `wait_timeout` defaults to 8 hours; shorter lifetime avoids stale connection errors. |
| `charset` | `utf8mb4` | Required for correct Unicode handling. |

**Docker Compose:**

```yaml
mysql:
  image: mysql:8.4
  container_name: cleat-mysql
  environment:
    MYSQL_ROOT_PASSWORD: cleat
    MYSQL_DATABASE: cleat
  ports:
    - "3306:3306"
  healthcheck:
    test: ["CMD", "mysqladmin", "ping", "-h", "localhost"]
    interval: 5s
    timeout: 5s
    retries: 10
    start_period: 30s
  volumes:
    - mysqldata:/var/lib/mysql
```

---

### 4.3 SQL Server

**Connection string format (URL style):**

```
sqlserver://username:password@host:port?database=dbname&connection+timeout=30&encrypt=false
```

Examples:

```
sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master&connection+timeout=30&encrypt=false
sqlserver://cleat:cleat@mssql:1433?database=cleat&connection+timeout=30&encrypt=true
```

ADO-style format (alternative):

```
Server=host,port;Database=dbname;User Id=user;Password=pass;Encrypt=false;
```

A helper function is available:

```go
func MSSQLConnectionString(host string, port int, user, password, database string) string {
    return fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s&connection+timeout=30&encrypt=false",
        user, password, host, port, database)
}
```

**Environment variable (used by tests):**

```
CLEAT_TEST_MSSQL=sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master
```

CI uses `database=master` because the test suite creates its own tables.
For production, specify your application database. Tests are skipped if
`CLEAT_TEST_MSSQL` is not set.

**Go driver:**

```go
import _ "github.com/microsoft/go-mssqldb"
```

**Factory setup:**

The MSSQL factory manages per-tenant connection pools with RLS session context
baked into every connection. The `tenantSessionConnector` wraps the driver's
connector to call `sp_set_session_context @key=N'tenant_id', @value=N'...'`
on every new connection.

```go
factory := host.NewMSSQLStoreFactory(
    "sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=cleat&connection+timeout=30",
)

// OpenStore creates a connection pool wrapped with the RLS connector.
store, closer, err := factory.OpenStore(ctx, tenantID, "default")
defer closer.Close()
```

The factory configures per-tenant connection pools with:

```go
tenantDB.SetMaxOpenConns(15)
tenantDB.SetMaxIdleConns(5)
tenantDB.SetConnMaxLifetime(5 * time.Minute)
```

**Key connection pool parameters:**

| Parameter | Recommended | Notes |
|-----------|-------------|-------|
| `SetMaxOpenConns` | 15 per tenant pool | Azure SQL Database throttles logins; stay conservative. |
| `SetMaxIdleConns` | 5 per tenant pool | Pool fragmentation is a concern with many tenants; avoid excessive idle connections. |
| `SetConnMaxLifetime` | 5 minutes | Azure SQL Gateway can drop idle connections; shorter lifetime ensures fresh connections. |
| `encrypt` | `false` for dev, `true` for production | Production should use TLS 1.2+ (Azure SQL requires it). |
| `connection+timeout` | 30 | Seconds to wait for initial connection. |

**Docker Compose:**

```yaml
mssql:
  image: mcr.microsoft.com/mssql/server:2022-latest
  container_name: cleat-mssql
  environment:
    ACCEPT_EULA: "Y"
    MSSQL_SA_PASSWORD: "CleatTest123!"
  ports:
    - "1433:1433"
  healthcheck:
    test: ["CMD", "/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost", "-U", "sa", "-P", "CleatTest123!", "-Q", "SELECT 1", "-C"]
    interval: 5s
    timeout: 5s
    retries: 15
    start_period: 30s
  volumes:
    - mssqldata:/var/opt/mssql
```

---

## 5. Tenant Isolation

### PostgreSQL — Row-Level Security

PostgreSQL uses `CREATE POLICY` with a session variable for tenant isolation.
This is a true database-enforced security boundary.

**How it works:**

1. **Session setup**: Each transaction calls `set_config('cleat.tenant_id', $1, true)`
   (`internal/host/db.go`, `setRLSOnTx`). This sets a session-local variable
   scoped to the current transaction.

2. **RLS policies**: Migration `002_tenant_foundation.sql` enables RLS on all
   tenant-scoped tables and creates policies with `USING` clauses:

   ```sql
   ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;

   CREATE POLICY tenant_isolation_instances ON workflow_instances
       FOR ALL USING (
           tenant_id = COALESCE(
               current_setting('cleat.tenant_id', true),
               '00000000-0000-0000-0000-000000000000'
           )::uuid
       );
   ```

3. **Fail-closed**: If no tenant context is set, `COALESCE` falls back to the
   default tenant UUID. Only superusers bypass RLS.

4. **Schema routing** (optional): The `PostgresStoreFactory` accepts a
   `schemaName` parameter and creates the schema on demand via
   `CREATE SCHEMA IF NOT EXISTS`. PostgreSQL's `search_path` DSN parameter
   routes queries to the correct schema.

### MySQL — Separate Database per Tenant

MySQL has no row-level security. Tenant isolation is enforced at the
application layer by scoping each tenant to its own database.

**How it works:**

1. **Per-tenant databases**: The `MySQLStoreFactory` creates a database per
   tenant: `cleat_<tenant_id>` (hyphens replaced with underscores). For
   example, tenant `550e8400-e29b-41d4-a716-446655440000` gets database
   `cleat_550e8400_e29b_41d4_a716_446655440000`.

2. **Dedicated connection pool**: Each tenant gets its own `*sql.DB` pool
   scoped to that tenant's database. The factory opens a new pool using the
   base DSN with the tenant database name inserted.

3. **Defense-in-depth**: Even though the connection is already scoped to the
   tenant's database, all queries retain `WHERE tenant_id = ?` clauses as an
   additional safeguard.

4. **Factory API**:
   - `CreateTenantDatabase(ctx, tenantID)` creates the database and returns
     a connection pool (idempotent — `CREATE DATABASE IF NOT EXISTS`).
   - `DropTenantDatabase(tenantID)` removes the database and closes the pool.
   - `OpenStore(ctx, tenantID, taskQueues...)` calls `CreateTenantDatabase`
     if no pool exists yet.

5. **Validation**: Tenant IDs must be valid UUIDs. This prevents SQL injection
   through backtick-quoted database identifiers in `CREATE DATABASE` statements.

### SQL Server — Row-Level Security via Security Policies

SQL Server implements RLS through a security policy bound to an inline
table-valued function (TVF) that checks `SESSION_CONTEXT()`.

**How it works:**

1. **Connection-level setup**: The `tenantSessionConnector` (`internal/host/mssql_store.go`)
   wraps every new connection and calls
   `EXEC sp_set_session_context @key=N'tenant_id', @value=N'<tenantID>'`
   at connection open time.

2. **Per-transaction defense-in-depth**: Each transactional method also calls
   `setSessionContext(tx)` as a belt-and-suspenders approach:

   ```go
   func (s *MSSQLStore) setSessionContext(tx *sql.Tx) error {
       _, err := tx.Exec(`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, s.tenantID)
       return err
   }
   ```

3. **Security predicate**: The inline TVF `fn_tenant_filter()` checks:

   ```sql
   CREATE FUNCTION dbo.fn_tenant_filter()
   RETURNS TABLE
   AS RETURN
       SELECT 1 AS fn_tenant_filter_result
       WHERE CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER) = tenant_id
          OR IS_MEMBER('db_owner') = 1;
   ```

4. **Security policy binding**: Each tenant-scoped table gets filter and block
   predicates via `CREATE SECURITY POLICY` (migration `002_tenant_foundation.sql`).

5. **db_owner bypass**: Members of `db_owner` role bypass the filter, which
   is standard SQL Server RLS behavior.

---

## 6. Migrating Between Backends

Cleat does **not** provide a cross-database data migration tool. Migrating
between backends requires draining the workflow queue and replaying workflow
state. The `WorkflowStore` interface and schema structure are equivalent across
all backends, but data must be recreated.

### Schema parity

All three backends share the same migration structure:

```
migrations/
  postgres/       001_initial_schema.sql   through   011_add_event_checksum.sql
  mysql/          001_initial_schema.sql   through   011_add_event_checksum.sql
  mssql/          001_initial_schema.sql   through   011_add_event_checksum.sql
```

Each migration number implements the same logical schema change, adapted for
the target dialect's syntax.

### PostgreSQL to MySQL

| Consideration | Details |
|---------------|---------|
| **No RETURNING** | MySQL does not support `UPDATE ... RETURNING`. The claim pattern uses three statements (SELECT FOR UPDATE SKIP LOCKED, UPDATE, SELECT) inside a single transaction. |
| **No RLS** | Tenant isolation switches to separate databases per tenant. See Section 5. |
| **No partial indexes** | Indexes that filter by `WHERE status = 'ready'` in PostgreSQL become full indexes on `(status, next_wake_at)`. Larger index, but query plans still use them effectively. |
| **No native SHA-256** | SHA-256 hashing for idempotency keys and event checksums is computed in Go (Go standard library `crypto/sha256`). |
| **No native percentiles** | `LoadMemoryStats` loads all samples and computes percentiles in Go. |
| **JSONB becomes JSON** | MySQL JSON is a binary JSON type. Use `JSON_EXTRACT` and `JSON_UNQUOTE` instead of PostgreSQL's `->` / `->>`. |
| **TIMESTAMPTZ becomes TIMESTAMP(6)** | Stored as UTC; no timezone awareness in the column type. Go driver handles conversion with `parseTime=true`. |
| **BYTEA becomes LONGBLOB** | Maximum 4 GB. Sufficient for WASM binaries and serialized state. |
| **BOOLEAN becomes TINYINT(1)** | `true` = 1, `false` = 0. |
| **TEXT[] becomes JSON** | Array fields (e.g., `entry_points`) use JSON arrays instead of native PostgreSQL arrays. |
| **Placeholder syntax** | `$1`, `$2` becomes `?`. |

### PostgreSQL to SQL Server

| Consideration | Details |
|---------------|---------|
| **No RETURNING** | SQL Server uses `UPDATE ... OUTPUT INSERTED.*` instead. This is a single-statement pattern — arguably cleaner than PostgreSQL's approach. |
| **RLS maps cleanly** | `set_config()` becomes `sp_set_session_context()`; `CREATE POLICY` becomes `CREATE SECURITY POLICY` with inline TVF. Same isolation model. |
| **LIKE is case-insensitive** | SQL Server's `LIKE` is case-insensitive by default with default collation, matching PostgreSQL's `ILIKE` behavior. |
| **OFFSET/LIMIT becomes OFFSET/FETCH** | Use `ORDER BY ... OFFSET 0 ROWS FETCH NEXT N ROWS ONLY`. |
| **ON CONFLICT becomes MERGE** | SQL Server uses `MERGE` for upsert. For idempotent insert, use `INSERT ... SELECT ... WHERE NOT EXISTS`. |
| **JSONB becomes NVARCHAR(MAX)** | JSON stored as Unicode text. Use `JSON_VALUE` / `JSON_QUERY` for extraction. |
| **TIMESTAMPTZ becomes DATETIMEOFFSET** | Full timezone-aware datetime type. |
| **BYTEA becomes VARBINARY(MAX)** | Handles binary data up to 2 GB. |
| **BOOLEAN becomes BIT** | `true` = 1, `false` = 0. |
| **TEXT becomes NVARCHAR(MAX)** | Unicode text type. |
| **Placeholder syntax** | `$1` becomes `@p1` (positional named parameters). |
| **now() becomes SYSUTCDATETIME()** | UTC-based current time with timezone offset. |
| **gen_random_uuid()** | `NEWID()` in T-SQL, or generate UUIDs in Go application code. |

### Data Type Translation Matrix

| PostgreSQL | MySQL | SQL Server | Go Type |
|------------|-------|------------|---------|
| `UUID` | `CHAR(36)` | `UNIQUEIDENTIFIER` | `string` (Go-generated UUID v4) |
| `TEXT` | `TEXT` / `VARCHAR(255)` | `NVARCHAR(MAX)` / `NVARCHAR(255)` | `string` |
| `JSONB` | `JSON` | `NVARCHAR(MAX)` | `json.RawMessage` |
| `BYTEA` | `LONGBLOB` | `VARBINARY(MAX)` | `[]byte` |
| `TIMESTAMPTZ` | `TIMESTAMP(6)` | `DATETIMEOFFSET` | `time.Time` |
| `BIGSERIAL` | `BIGINT AUTO_INCREMENT` | `BIGINT IDENTITY(1,1)` | `int64` |
| `BOOLEAN` | `TINYINT(1)` | `BIT` | `bool` |
| `DOUBLE PRECISION` | `DOUBLE` | `FLOAT(53)` | `float64` |
| `TEXT[]` | `JSON` (array) | `NVARCHAR(MAX)` (JSON array) | `[]string` (via JSON) |

---

## 7. Known Limitations

### 7.1 MySQL

1. **No RETURNING clause**: The atomic claim pattern requires three statements
   (SELECT FOR UPDATE SKIP LOCKED to lock candidate IDs, UPDATE to claim them,
   SELECT to read back the full rows) instead of PostgreSQL's single
   `UPDATE ... RETURNING`. The transaction ensures atomicity, but there is a
   marginal performance cost from the extra round trips.

2. **No Row-Level Security**: Tenant isolation depends on separate databases
   per tenant and application-level `WHERE tenant_id = ?` clauses. A missing
   clause in a query could cause a cross-tenant data leak. The defense-in-depth
   approach of scoping the entire connection pool to one database per tenant
   mitigates this significantly.

3. **No partial (filtered) indexes**: Indexes in migration 002 are full
   indexes instead of filtered indexes. The `idx_instances_tenant_ready` index
   covers all statuses, not just `ready`, making it larger than the equivalent
   PostgreSQL or SQL Server index. Query plans still use it effectively.

4. **No native SHA-256**: Idempotency key hashing and event checksums are
   computed in Go using `crypto/sha256`. No measurable performance impact.

5. **No native percentiles**: `LoadMemoryStats` loads all memory samples for
   a definition and computes percentiles in Go. For definitions with very
   large sample counts, consider rate-limiting or pre-aggregation.

6. **`TEXT` primary key limitation**: MySQL requires explicit prefix lengths
   on `TEXT` columns used as indexes or primary keys. Workflow instance IDs
   and similar columns use `VARCHAR(255)` instead of `TEXT` to avoid this
   limitation.

7. **No `DROP INDEX IF EXISTS`**: Migration scripts must ensure indexes
   exist before dropping them. The MySQL migration files include comments
   noting this requirement.

8. **`DESC` in index definitions ignored**: MySQL ignores sort direction in
   `CREATE INDEX` for most purposes (InnoDB). Index definitions that use
   `DESC` in PostgreSQL become simple multi-column indexes.

### 7.2 SQL Server

1. **`READPAST` and snapshot isolation**: `READPAST` skips rows locked by
   other transactions, but under `READ COMMITTED SNAPSHOT` isolation, readers
   do not block writers — `READPAST` may not provide the expected skip-locked
   behavior for read-only queries. The implementation uses `WITH (READPAST,
   UPDLOCK, ROWLOCK)` on all claim queries to ensure correct skip-locked
   behavior with update locks.

2. **`UNIQUEIDENTIFIER` performance**: SQL Server's `UNIQUEIDENTIFIER` (UUID)
   type is 16 bytes but is not sequentially ordered. When used as a clustered
   index key, inserts cause page splits. Cleat uses `NVARCHAR(64)` for
   workflow instance IDs (not `UNIQUEIDENTIFIER`), avoiding this issue.

3. **No `CREATE TABLE IF NOT EXISTS`**: Table creation uses a wrapper pattern
   with `IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = ...)` before
   each `CREATE TABLE` statement. Migration files contain these wrappers.

4. **`STRING_SPLIT` output order**: `STRING_SPLIT` does not guarantee output
   order. Task queue filtering uses `STRING_SPLIT` for `IN`-clause emulation;
   the ordering does not affect correctness.

5. **`sp_set_session_context` parameter typing**: The `@value` parameter
   expects `NVARCHAR(128)` / `SQL_VARIANT`. The tenant ID is passed as a Go
   string and the TVF predicate casts it to `UNIQUEIDENTIFIER`.

6. **Azure SQL Database specifics**:
   - No `USE` statement available; the connection string specifies the database.
   - Mandatory TLS 1.2.
   - Connection resiliency: transient fault handling is essential; Azure SQL
     Gateway can drop idle connections.
   - Database-level settings (`MAXDOP`, `cost threshold for parallelism`) are
     managed through Azure portal or `ALTER DATABASE SCOPED CONFIGURATION`.

### 7.3 All Backends

1. **No cross-backend data migration tool**: Switching backends requires
   draining the workflow queue and replaying state.

2. **Memory statistics accuracy**: Percentile computations (MySQL) and EWMA
   estimates (all backends) are approximate. PostgreSQL and SQL Server have
   native window functions that are exact; MySQL computes from all samples in
   Go.

---

## 8. Performance Tuning

### 8.1 Connection Pool Sizing

The factory defaults are conservative. Tune based on your workload:

| Scenario | MaxOpenConns | MaxIdleConns | ConnMaxLifetime |
|----------|-------------|-------------|-----------------|
| Light (10 workflows/sec) | 10 | 5 | 30 minutes |
| Moderate (100 workflows/sec) | 25 | 10 | 30 minutes |
| Heavy (1000+ workflows/sec) | 50-100 | 25 | 15 minutes |

For MySQL and SQL Server, each tenant gets its own pool. The total database
connections = (number of tenants) x (MaxOpenConns). Plan accordingly.

### 8.2 Index Strategy

All required indexes are created by the migration files. The critical indexes
for workflow dispatch:

| Purpose | Columns | Partial filter | Backend |
|---------|---------|---------------|---------|
| Ready queue polling | `(tenant_id, status, next_wake_at)` | `WHERE status = 'ready'` | PostgreSQL, SQL Server |
| Ready queue polling | `(tenant_id, status, next_wake_at)` | Full index (no filter) | MySQL |
| Heartbeat updates | `(assigned_to, heartbeat_at)` | `WHERE status = 'running'` | PostgreSQL, SQL Server |
| Heartbeat updates | `(assigned_to, heartbeat_at)` | Full index | MySQL |
| Stale instance reaper | `(status, heartbeat_at)` | `WHERE status = 'running'` | PostgreSQL, SQL Server |
| Stale instance reaper | `(status, heartbeat_at)` | Full index | MySQL |
| Sticky worker | `(sticky_worker_id)` | `WHERE sticky_worker_id IS NOT NULL` | PostgreSQL, SQL Server |
| Sticky worker | `(sticky_worker_id)` | Full index | MySQL |

### 8.3 PostgreSQL-Specific Tuning

**work_mem:**

Set per operation memory. Too low causes disk spills during sorts (the claim
query sorts by sticky worker priority and creation time):

```ini
work_mem = 16MB
```

**effective_cache_size:**

Tell the planner how much filesystem cache is available:

```ini
effective_cache_size = 4GB   # 50-75% of total RAM
```

**maintenance_work_mem:**

For `VACUUM` and `CREATE INDEX` operations:

```ini
maintenance_work_mem = 512MB
```

**Autovacuum tuning:**

The `workflow_instances` table has high update volume (status changes,
heartbeat updates). Aggressive autovacuum prevents bloat:

```ini
autovacuum_vacuum_scale_factor = 0.01
autovacuum_analyze_scale_factor = 0.005
```

### 8.4 MySQL-Specific Tuning

**InnoDB buffer pool:**

Set to 70-80% of available RAM. This is the single most important MySQL
performance parameter:

```ini
[mysqld]
innodb_buffer_pool_size = 8G
innodb_buffer_pool_instances = 4  # 1 instance per ~2 GB of pool
```

**Skip locked contention:**

Ensure `idx_instances_tenant_ready` exists on `workflow_instances(tenant_id,
status, next_wake_at)`. Without this index, `SELECT ... FOR UPDATE SKIP LOCKED`
performs a full table scan under lock, serializing all claim attempts.

**Connection timeouts:**

Set MySQL `wait_timeout` higher than the Go `ConnMaxLifetime`:

```ini
[mysqld]
wait_timeout = 600        # 10 minutes
interactive_timeout = 600
max_connections = 500      # Adjust based on (tenant count) x (pool size)
```

**Transaction isolation:**

Use `READ COMMITTED` to reduce gap locking:

```ini
[mysqld]
transaction_isolation = READ-COMMITTED
```

### 8.5 SQL Server-Specific Tuning

**READ COMMITTED SNAPSHOT ISOLATION (RCSI):**

Enable RCSI to reduce contention between readers and writers:

```sql
ALTER DATABASE cleat SET READ_COMMITTED_SNAPSHOT ON;
ALTER DATABASE cleat SET ALLOW_SNAPSHOT_ISOLATION ON;
```

Note: Under RCSI, the claim queries use `UPDLOCK` + `READPAST` together to
ensure correct skip-locked behavior. Test your workload after enabling RCSI.

**Tempdb configuration:**

Tempdb can become a bottleneck under heavy concurrent load. Use multiple data
files (one per CPU core, up to 8) of equal size:

```sql
ALTER DATABASE tempdb ADD FILE (
    NAME = tempdev2,
    FILENAME = 'C:\temp\tempdb2.ndf',
    SIZE = 1024MB,
    FILEGROWTH = 512MB
);
```

**MAXDOP:**

For OLTP workloads, limit parallelism:

```sql
sp_configure 'max degree of parallelism', 4;
RECONFIGURE;
```

**Cost threshold for parallelism:**

Raise the default (5) to avoid trivial queries getting parallel plans:

```sql
sp_configure 'cost threshold for parallelism', 50;
RECONFIGURE;
```

**Azure SQL Database:**

Azure SQL automatically manages tempdb and most system-level settings. Focus on:
- Right-size DTU / vCore (monitor via `sys.dm_db_resource_stats`)
- Connection retry logic (Azure SQL Gateway can drop connections)
- `encrypt=true` in connection string (mandatory for Azure SQL)

---

## 9. CI / Testing Your Backend

### How Tests Detect Backends

Tests in `internal/host/...` use environment variables via the `TestDB` helper
in `internal/host/testutil/schema.go`:

| Backend | Environment Variable | Default |
|---------|---------------------|---------|
| PostgreSQL | `CLEAT_TEST_POSTGRES` | `postgres://localhost:5432/cleat?sslmode=disable` |
| MySQL | `CLEAT_TEST_MYSQL` | (skipped if not set) |
| SQL Server | `CLEAT_TEST_MSSQL` | (skipped if not set) |

Fallback for PostgreSQL: `CLEAT_TEST_DB` (legacy).

All database tests are skipped in short mode (`go test -short`).

### Running Tests Locally

**Start the databases:**

The project provides a Docker Compose file at `docker-compose.cluster.yml`
that runs all three databases:

```bash
docker compose -f docker-compose.cluster.yml up -d postgres mysql mssql
```

**Run tests against a specific backend:**

```bash
# PostgreSQL (always runs if PostgreSQL is available)
CLEAT_TEST_POSTGRES="postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
  go test -count=1 -timeout=300s ./internal/host/...

# MySQL
CLEAT_TEST_MYSQL="root:cleat@tcp(127.0.0.1:3306)/cleat?parseTime=true&multiStatements=true" \
  go test -count=1 -timeout=300s ./internal/host/...

# SQL Server
CLEAT_TEST_MSSQL="sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master&connection+timeout=30" \
  go test -count=1 -timeout=300s ./internal/host/...
```

**Run against all three backends:**

```bash
CLEAT_TEST_POSTGRES="postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
CLEAT_TEST_MYSQL="root:cleat@tcp(127.0.0.1:3306)/cleat?parseTime=true&multiStatements=true" \
CLEAT_TEST_MSSQL="sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master&connection+timeout=30" \
  go test -race -count=1 -timeout=300s ./internal/host/...
```

### CI Workflow

The CI pipeline (`.github/workflows/multi-db-ci.yml`) runs MySQL and SQL Server
tests using GitHub Actions service containers:

```yaml
jobs:
  test-mysql:
    services:
      mysql:
        image: mysql:8.4
        env:
          MYSQL_ROOT_PASSWORD: cleat
          MYSQL_DATABASE: cleat
    env:
      CLEAT_TEST_MYSQL: root:cleat@tcp(127.0.0.1:3306)/cleat

  test-mssql:
    services:
      mssql:
        image: mcr.microsoft.com/mssql/server:2022-latest
        env:
          ACCEPT_EULA: Y
          MSSQL_SA_PASSWORD: CleatTest123!
    env:
      CLEAT_TEST_MSSQL: sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master
```

PostgreSQL tests run as part of the main CI suite. MySQL and SQL Server have
dedicated CI jobs that trigger on pushes to `main` and `feature/**` branches,
and on pull requests targeting `main`.

### Test Infrastructure Reference

Test helpers are in `internal/host/testutil/schema.go`:

| Function | Description |
|----------|-------------|
| `TestDB(t, dialect)` | Opens a connection for the given dialect using the appropriate env var, creates minimal schema, and returns `*sql.DB`. |
| `SetupMinimalSchema(t, db, dialect)` | Creates the four core tables (idempotent): `workflow_defs`, `workflow_instances`, `event_history`, `workflow_signals`. |
| `SetupFullSchema(t, db, dialect)` | Calls `SetupMinimalSchema` then adds schedules, concurrency_keys, workflow_promises, memory stats tables, and all indexes. |
| `CleanupTestData(t, db, dialect, runID)` | Removes test data matching a given ID pattern (e.g., `"test-%"`) from all tables. |

### Migration Directory Structure

```
migrations/
  postgres/       10 files (001 through 011)
  mysql/          10 files (001 through 011)
  mssql/          10 files (001 through 011)
```

| Migration | Purpose |
|-----------|---------|
| `001_initial_schema.sql` | Core tables: `workflow_defs`, `workflow_instances`, `event_history`, `workflow_signals` |
| `002_tenant_foundation.sql` | Tenant metadata tables, tenant_id columns, RLS policies, tenant-scoped indexes |
| `003_task_routing.sql` | Task queue columns, sticky worker support |
| `005_exactly_once.sql` | Idempotency keys, deduplication infrastructure |
| `006_history_compaction.sql` | Event history compaction state |
| `007_jsonb_payload.sql` | JSON payload columns on event history |
| `008_workflow_versioning.sql` | Workflow definition version management |
| `009_tenant_roles.sql` | Tenant provisioning roles and permissions |
| `010_workflow_memory_stats.sql` | Memory sampling and EWMA statistics tables |
| `011_add_event_checksum.sql` | SHA-256 checksum column for event integrity verification |

---

## Quick Reference

| | PostgreSQL | MySQL / MariaDB | SQL Server |
|--|-----------|-----------------|------------|
| **Go driver** | `github.com/lib/pq` | `github.com/go-sql-driver/mysql` | `github.com/microsoft/go-mssqldb` |
| **Driver name** | `"postgres"` | `"mysql"` | `"mssql"` |
| **Dialect constant** | `DialectPostgres` | `DialectMySQL` | `DialectMSSQL` |
| **Placeholder** | `$1`, `$2`, ... | `?` | `@p1`, `@p2`, ... |
| **Factory type** | `PostgresStoreFactory` | `MySQLStoreFactory` | `MSSQLStoreFactory` |
| **Factory input** | `*sql.DB` + schema name | `*sql.DB` + base DSN string | Connection string |
| **Now function** | `now()` | `NOW(6)` | `SYSUTCDATETIME()` |
| **Pool defaults** | 25 / 10 / 30 min | 15 / 5 / 5 min | 15 / 5 / 5 min |
| **Tenant isolation** | RLS (set_config + policies) | Separate databases | RLS (sp_set_session_context + security policies) |
| **Enum env var** | `CLEAT_TEST_POSTGRES` | `CLEAT_TEST_MYSQL` | `CLEAT_TEST_MSSQL` |
