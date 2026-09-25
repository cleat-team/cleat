package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
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

// rebindArgs rewrites a PostgreSQL-shaped statement AND reorders its args
// for this dialect. Every call site in this package hands its result
// straight to a raw *sql.DB/*sql.Tx/*sql.Conn -- cleatctl has no
// plugin.PluginDB adapter to do this centrally -- so this is the one place
// in the package responsible for it, the same role
// engine/plugindb_adapter.go's SQLDBAdapter plays for plugins.
//
// This is not optional on MySQL (cleat#2259): plugin.Rebind is the identity
// there, so a caller that used rebind (or plugin.Rebind directly) instead
// of this would send literal "$1" text to the driver, which is a syntax
// error, not a silent mis-bind -- loud, but still a break every one of
// these call sites would have hit the first time it ran against MySQL.
func (d dialect) rebindArgs(q string, args ...any) (string, []any, error) {
	return plugin.RebindArgs(q, d.query, args)
}

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

// tenantScopedDB returns the *sql.DB that holds tenantID's per-tenant
// tables.
//
// On PostgreSQL and SQL Server that is db itself: tenant isolation there is
// RLS / a filter predicate inside one shared database, and db already
// carries the right role and connection state for it. On MySQL it can be a
// DIFFERENT physical database, cleat_<tenant-id> -- MySQL has no RLS, so
// cleat isolates SOME tables with one database per tenant instead
// (MySQLStoreFactory.CreateTenantDatabase).
//
// Which database a table is authoritative in is a property of the READER,
// not of the migration that created it: every table's schema is applied to
// both the base database and each tenant database, so a migration header
// cannot settle this on its own. queues/queue_holders are confirmed
// per-tenant -- ClaimWorkflows's claim query joins them against
// workflow_instances unqualified in one statement, which only resolves if
// both live in the same connected database. tenant_secrets,
// tenant_egress_allow, tenant_api_keys and tenant_settings are confirmed
// control-plane: cleat-worker reads all four on the base db it opens at
// startup, before it has routed to any tenant.
//
// Any subcommand that reads or writes a per-tenant table must call this
// rather than use db directly, or its write lands in whatever database
// --db happened to name and cleat-worker never sees it: that was
// cleat#1956, found because `cleatctl queue create` reported success and
// `cleatctl queue list` read the row straight back -- from the same wrong
// database, so the round-trip was not a control on the question that
// mattered. The first version of #1956 also listed tenant_secrets as
// per-tenant by reasoning from its migration header alone; that was wrong,
// caught by checking cleat-worker's actual read path instead, and is the
// reason this comment states the rule as "ask the reader" rather than
// listing tables by category.
func (d dialect) tenantScopedDB(ctx context.Context, db *sql.DB, dsn, tenantID string) (*sql.DB, error) {
	if d.name != "mysql" {
		return db, nil
	}
	tdb, err := engine.NewMySQLStoreFactory(db, mysqlBaseDSN(dsn)).CreateTenantDatabase(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("open tenant %s's database: %w", tenantID, err)
	}
	return tdb, nil
}

// tenantRuntimeQualifier returns the SQL qualifier that names the database
// holding tenantID's per-tenant tables, and whether that database is there.
//
// It is the READ-ONLY counterpart of tenantScopedDB, and the difference is the
// whole reason it exists: tenantScopedDB routes through CreateTenantDatabase,
// which CREATES the database when it is missing. That is right for a command
// that is about to write. It is wrong for a diagnostic -- check-db asking
// "does this deployment have its runtime database" must not answer by making
// one, then reporting the database it just made as present. A check that
// repairs what it measures cannot report the fault.
//
// The empty qualifier that comes back on PostgreSQL and SQL Server is not a
// failure: those isolate tenants inside one database with RLS or a filter
// predicate, so an unqualified name already resolves to the right rows, and
// `found` is true because there is nothing separate to look for.
//
// The returned qualifier is a backtick-quoted identifier followed by a dot,
// meant to be concatenated onto a table name. Interpolating an identifier is
// safe here ONLY because tenantID is parsed as a UUID first; that check is not
// decoration, it is what makes the concatenation legitimate.
func (d dialect) tenantRuntimeQualifier(ctx context.Context, db *sql.DB, tenantID string) (string, bool, error) {
	if d.name != "mysql" {
		return "", true, nil
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		return "", false, fmt.Errorf("invalid tenant ID %q: %w", tenantID, err)
	}
	name := engine.MySQLTenantDatabaseName(tenantID)
	var n int
	// Written with a $1 placeholder and rebound rather than with MySQL's `?`,
	// even though this branch only ever runs on MySQL. information_schema.schemata
	// is one of the few catalogue views spelled the same on both, so the
	// PostgreSQL form is a real statement -- which keeps it inside
	// TestEveryInlineStatementParsesOnPostgres instead of needing a pin there.
	// A pin would have been the easy route and it would have removed the only
	// check that this statement can run at all.
	stmt, stmtArgs, err := d.rebindArgs(`SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = $1`, name)
	if err != nil {
		return "", false, fmt.Errorf("rebind tenant database lookup for %s: %w", name, err)
	}
	if err := db.QueryRowContext(ctx, stmt, stmtArgs...).Scan(&n); err != nil {
		return "", false, fmt.Errorf("look for tenant database %s: %w", name, err)
	}
	if n == 0 {
		return "", false, nil
	}
	return "`" + name + "`.", true, nil
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
