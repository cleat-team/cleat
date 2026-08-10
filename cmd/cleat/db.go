package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// openPostgresDB is the only place in this package that may call sql.Open.
// Every DB-touching subcommand -- deploy, versions, rollback, schedule, lock,
// and plugin install/list/update/uninstall -- routes through it.
// db_open_guard_test.go enforces that with a static check over the package's
// AST, so a new call site cannot reintroduce the gap this file closes.
//
// Why it exists: cmd/cleat registers only the "postgres" driver (via
// github.com/lib/pq, imported transitively through package engine) and, before
// this change, called sql.Open("postgres", connStr) unconditionally at ten
// call sites regardless of what the caller actually passed. tiers.yaml claims
// engine support for postgres, mysql and mssql, and README.md said the same
// for the CLI -- but nothing in cmd/cleat ever checked the dialect
// (`grep -ci 'mysql\|mssql' cmd/cleat/*.go` -> 0), so a MySQL or SQL Server
// DSN reached lib/pq and produced whatever lib/pq made of a string it was
// never built to parse. Measured 2026-08-09 with this binary, pre-fix:
//
//	MySQL DSN   root:pass@tcp(127.0.0.1:3306)/cleat
//	  -> `missing "=" after "root:pass@tcp(127.0.0.1:3306)/cleat" in connection info string`
//	SQL Server  sqlserver://sa:Password1@localhost:1433?database=cleat
//	  -> `pq: SSL is not enabled on the server`
//
// The SQL Server case is the more dangerous of the two: lib/pq didn't reject
// the string, it fell through to default host/port parsing and produced an
// error about SSL that has nothing to do with the actual problem, which is
// dialect. Both replaced by the message below.
//
// This CLI's DB-touching subcommands only ever talk to PostgreSQL. The
// engine's own multi-dialect support is real (tiers.yaml) but the CLI has
// never had a code path for it; cmd/deploy-workflow is the one place that
// does (--driver postgres|mysql|mssql), and it covers `deploy` only -- it has
// no equivalent of `versions`, `rollback`, or `plugin`.
func openPostgresDB(connStr string) (*sql.DB, error) {
	if d, ok := detectNonPostgresDialect(connStr); ok {
		return nil, fmt.Errorf(
			"this connection string looks like %s, but the cleat CLI (deploy/versions/rollback/schedule/lock/plugin) only supports PostgreSQL today. "+
				"The cleat engine also supports %s (see tiers.yaml), but the CLI does not talk to it yet -- the only multi-dialect entry point is `deploy-workflow --driver %s` (deploy only; it has no versions/rollback/plugin equivalent). "+
				"Point --db / CLEAT_DATABASE_URL at a PostgreSQL connection string instead",
			d.name, d.name, d.deployWorkflowFlag)
	}
	return sql.Open("postgres", connStr)
}

// dialectHint names a non-Postgres dialect a connection string appears to
// target, and the cmd/deploy-workflow --driver value that can actually reach
// it.
type dialectHint struct {
	name               string // human-readable, e.g. "SQL Server"
	deployWorkflowFlag string // cmd/deploy-workflow's --driver value
}

// detectNonPostgresDialect reports whether connStr looks like a MySQL or SQL
// Server DSN rather than a PostgreSQL one.
//
// This is a heuristic aimed at the DSN shapes people actually paste -- a
// mysql:// or sqlserver:// URL, a JDBC SQL Server URL, or the Go MySQL
// driver's user:pass@tcp(host:port)/db form -- not a validating parser. A
// string this function does not flag is not thereby proven to be a valid
// Postgres DSN; that remains lib/pq's and Ping's job, same as before this
// change.
func detectNonPostgresDialect(connStr string) (dialectHint, bool) {
	lower := strings.ToLower(strings.TrimSpace(connStr))
	switch {
	case strings.HasPrefix(lower, "sqlserver://"),
		strings.HasPrefix(lower, "mssql://"),
		strings.HasPrefix(lower, "jdbc:sqlserver:"):
		return dialectHint{name: "SQL Server", deployWorkflowFlag: "mssql"}, true
	case strings.HasPrefix(lower, "mysql://"),
		strings.Contains(lower, "@tcp("):
		return dialectHint{name: "MySQL", deployWorkflowFlag: "mysql"}, true
	default:
		return dialectHint{}, false
	}
}
