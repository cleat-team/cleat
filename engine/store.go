package engine

import (
	"context"
	"io"
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
