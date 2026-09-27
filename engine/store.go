package engine

import (
	"context"
	"io"
	"time"
)

// Dialect identifies the SQL dialect of a database backend.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectMySQL    Dialect = "mysql"
	DialectMSSQL    Dialect = "mssql"
)

// StoreFactory creates WorkflowStore instances. Each database backend
// implements one. The factory encapsulates connection management, schema
// setup, and backend-specific configuration — callers never need to know
// whether the store is backed by PostgreSQL, MySQL, or SQLite.
type StoreFactory interface {
	// OpenStore creates or connects to a WorkflowStore scoped to the given tenant.
	// The tenantID identifies which tenant the store should operate on.
	// The taskQueues slice specifies which queues this store should poll.
	OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error)

	// DriverName returns the database/sql driver name for health checks.
	DriverName() string

	// Dialect returns the SQL dialect of this factory's backend.
	Dialect() Dialect
}

// PerTenantPooler is implemented by a StoreFactory that opens a CONNECTION
// POOL per tenant rather than sharing one.
//
// The distinction is not cosmetic and it is not uniform across dialects:
//
//   - PostgresStoreFactory shares a single *sql.DB. A tenant costs a struct;
//     the tenant is supplied per transaction through cleat.tenant_id.
//   - MSSQLStoreFactory cannot share one. Its row-level security reads
//     SESSION_CONTEXT, which is set per CONNECTION, so the tenant is a
//     property of the connection and each one needs its own pool.
//   - MySQLStoreFactory gives each tenant its own database, so likewise.
//
// It exists because cmd/cleat-worker's connection census had no way to ask.
// It reported the per-tenant term as zero unless --tenant-isolation=role had
// built plugin.TenantPools -- which is PostgreSQL-only -- and so reported zero
// on precisely the two dialects that always have the term. That is the defect
// cleat#1486 was filed for ("a worker opens six independent pools and no code
// anywhere added them up"), recurring in the one term a worker serving many
// tenants notices first.
//
// Returns the per-tenant ceiling. A factory that shares one pool does not
// implement this interface at all, rather than returning 0: "no per-tenant
// pools" and "per-tenant pools with a ceiling of zero" are different answers,
// and an absent method cannot be mistaken for either.
type PerTenantPooler interface {
	TenantPoolMaxConns() int
}

// TenantPoolReaper is implemented by a StoreFactory whose per-tenant pools can
// be released while the process keeps running.
//
// It is the counterpart to plugin.TenantPools.EvictIdle, which cmd/cleat-worker
// has reaped on a timer since cleat#1470. The store factories had no equivalent:
// MySQLStoreFactory and MSSQLStoreFactory released a tenant's pool only at
// Close(), so a worker that served a tenant once kept its *sql.DB and the
// connection-opener goroutine behind it for the life of the process. Small per
// tenant, unbounded in tenants, and invisible from either side -- the role pools
// had a reaper and the store pools did not, and nothing said so.
//
// # The contract a caller has to keep
//
// EvictIdle closes pools, and a store handed out by OpenStore holds its *sql.DB
// directly. The closer OpenStore returns is therefore a LEASE: a pool with an
// outstanding lease is never evicted, however idle it looks. Any caller holding
// a store for longer than the idle window -- a workflow execution, a worker's
// process-wide store -- must hold that closer for exactly as long, or its next
// query meets a closed pool.
//
// A factory that shares one pool across tenants does not implement this, rather
// than implementing it as a no-op: "nothing to reap" and "reaped nothing this
// time" are different answers, and an absent method cannot be mistaken for
// either. Same reasoning as PerTenantPooler above.
type TenantPoolReaper interface {
	// EvictIdle closes unleased pools untouched for maxIdle and reports how
	// many. A non-positive maxIdle evicts nothing.
	EvictIdle(maxIdle time.Duration) int

	// TenantPoolCount is how many pools are open right now. It exists so a
	// test can prove an eviction happened rather than trusting the count
	// EvictIdle returns about itself.
	TenantPoolCount() int
}
