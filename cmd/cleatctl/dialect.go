package main

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// dialect is the database cleatctl is pointed at.
//
// cleatctl carried `sql.Open("postgres", ...)` as its only connection until
// cleat#1316, so replay, debug and check-db -- the POST-INCIDENT tools -- were
// unavailable on two of the three backends the engine supports. An operator on
// SQL Server could not replay the workflow that had just diverged.
type dialect struct {
	// name is what a human called it: "postgres", "mysql", "mssql". It is also
	// the cleat-worker --driver value, deliberately, so that an error message
	// naming a dialect names something the reader can pass to another tool.
	name string

	// driver is the database/sql driver name, which is NOT the same string:
	// "mssql" is registered as "sqlserver". This mismatch is the reason
	// cleat-worker has sqlDriverName, and the reason this is a field rather
	// than a string the caller is trusted to convert.
	driver string

	// query is plugin.Dialect, which is what plugin.Rebind and plugin.Query
	// take. Every $N in this package is rewritten through it rather than
	// rewritten by hand: Rebind is delimiter-aware, so a $1 inside a string
	// literal or a comment is left alone, which a regex over the statement
	// would not manage.
	query plugin.Dialect
}

var (
	dialectPostgres = dialect{name: "postgres", driver: "postgres", query: plugin.DialectPostgres}
	dialectMySQL    = dialect{name: "mysql", driver: "mysql", query: plugin.DialectMySQL}
	dialectMSSQL    = dialect{name: "mssql", driver: "sqlserver", query: plugin.DialectMSSQL}
)

// rebind rewrites a PostgreSQL-shaped statement for this dialect.
//
// Statements in this package are written with $N and now(), the PostgreSQL
// forms, and rewritten here. Where a statement cannot be rewritten -- a cast,
// an information_schema column that does not exist -- it is a plugin.Query
// with an explicit arm instead, because a rewrite that silently does nothing
// is worse than one that is not attempted.
func (d dialect) rebind(q string) string { return plugin.Rebind(q, d.query) }

// detectDialect infers the dialect from the DSN's shape.
//
// A heuristic over the DSN forms people paste, not a validating parser -- the
// same judgement cmd/cleat's detectNonPostgresDialect makes, and for the same
// reason: a string this does not recognise is not thereby proven to be valid
// PostgreSQL, which stays the driver's and Ping's job.
//
// PostgreSQL is the default rather than an error because every DSN cleatctl
// accepted before this existed was a PostgreSQL one, and a tool that starts
// refusing input it used to take is a worse outcome than one that guesses the
// way it always did. An explicit --driver overrides it.
func detectDialect(dsn string) dialect {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case strings.HasPrefix(lower, "sqlserver://"),
		strings.HasPrefix(lower, "mssql://"),
		strings.HasPrefix(lower, "jdbc:sqlserver:"):
		return dialectMSSQL
	case strings.HasPrefix(lower, "mysql://"),
		strings.Contains(lower, "@tcp("):
		return dialectMySQL
	default:
		return dialectPostgres
	}
}

// dialectByName resolves an explicit --driver value.
func dialectByName(name string) (dialect, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "postgres", "postgresql":
		return dialectPostgres, nil
	case "mysql":
		return dialectMySQL, nil
	case "mssql", "sqlserver":
		return dialectMSSQL, nil
	default:
		return dialect{}, fmt.Errorf("unknown driver %q: expected postgres, mysql or mssql", name)
	}
}

// openStoreFactory builds the engine store factory for this dialect.
//
// The three factories do not take the same things -- Postgres and MySQL take
// an open *sql.DB, SQL Server takes the connection string and opens its own
// pools -- which is why this returns the factory rather than letting each
// caller assemble one.
func (d dialect) openStoreFactory(db *sql.DB, dsn, schemaName string) (engine.StoreFactory, error) {
	switch d.name {
	case "postgres":
		return engine.NewPostgresStoreFactory(db, schemaName), nil
	case "mysql":
		return engine.NewMySQLStoreFactory(db, mysqlBaseDSN(dsn)), nil
	case "mssql":
		return engine.NewMSSQLStoreFactory(dsn), nil
	}
	return nil, fmt.Errorf("no store factory for dialect %q", d.name)
}

// mysqlBaseDSN strips the database name from a MySQL DSN, which is what
// NewMySQLStoreFactory expects: it appends a per-tenant database itself.
//
//	"root:pass@tcp(host:3306)/mydb?parseTime=true"
//	  -> "root:pass@tcp(host:3306)/?parseTime=true"
func mysqlBaseDSN(dsn string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	afterSlash := dsn[slash+1:]
	qIdx := strings.IndexByte(afterSlash, '?')
	if qIdx < 0 {
		return dsn[:slash+1]
	}
	return dsn[:slash+1] + afterSlash[qIdx:]
}

// qualifiedTable maps one entry of coreTables -- which is written in the
// PostgreSQL spelling, because that is what TestCoreTablesMatchTheMigrations
// validates it against -- onto the schema and table this dialect actually uses.
//
// The three disagree, and not in a way a placeholder rewrite can reach:
//
//	                 postgres            mysql              mssql
//	admin.tenants    schema `admin`      table `tenants`    schema `admin`
//	workflow_defs    schema `public`     the database       schema `dbo`
//
// MySQL has no schema-inside-a-database, so the `admin.` prefix is not a
// qualifier there at all -- `admin.tenants` names a DATABASE called admin and
// fails with "Unknown database 'admin'". Its migrations create bare `tenants`.
// SQL Server keeps `admin` and puts everything else in `dbo`.
//
// The empty schema for MySQL means "whichever database the DSN selected",
// which is the correct question to ask information_schema there.
func (d dialect) qualifiedTable(table string) (schema, name string) {
	adminSchema, bare, qualified := strings.Cut(table, ".")
	if !qualified {
		bare = table
		adminSchema = ""
	}
	switch d.name {
	case "mysql":
		// Both the admin tables and the rest live in the one database.
		return "", bare
	case "mssql":
		if adminSchema != "" {
			return adminSchema, bare
		}
		return "dbo", bare
	default:
		if adminSchema != "" {
			return adminSchema, bare
		}
		return "public", bare
	}
}
