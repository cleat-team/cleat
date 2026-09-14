package main

import (
	"context"
	"database/sql"
	"fmt"
)

// serverConnectionLimit is what the database will actually let this worker's
// role open, which is not the same as max_connections.
//
// cleat#1487 opens with "two default workers want 150 and PostgreSQL's default
// is 100", and nothing in the worker detects it: the four matches for
// `max_connections` in this tree are log labels for --max-plugin-connections,
// not the server setting. This type is the missing half of that sentence.
type serverConnectionLimit struct {
	// Max is the configured ceiling.
	Max int

	// Reserved is the slots this worker's role cannot have. On PostgreSQL that
	// is superuser_reserved_connections plus (since 16) reserved_connections --
	// both are subtracted from max_connections BEFORE an ordinary role is
	// admitted, so a stock container advertising 100 actually offers 97.
	//
	// Getting this wrong is how a diagnosis becomes a lie by three: a worker
	// needing exactly the advertised number would be told it fits.
	Reserved int

	// Detail names the settings read, so the log line can be checked against
	// the server rather than believed.
	Detail string
}

// Usable is what an ordinary role may actually open.
func (l serverConnectionLimit) Usable() int {
	u := l.Max - l.Reserved
	if u < 0 {
		return 0
	}
	return u
}

// queryServerConnectionLimit asks the server for its ceiling.
//
// Returns ok=false with a reason for dialects where the question does not
// arise, rather than inventing a number. SQL Server's ceiling is effectively
// unlimited (32767 against PostgreSQL's default 100), so a limit check there
// would be arithmetic about a bound nobody reaches.
func queryServerConnectionLimit(ctx context.Context, db *sql.DB, driver string) (serverConnectionLimit, bool, string) {
	switch driver {
	case "postgres":
		var maxConns, suReserved, reserved int
		// reserved_connections is PostgreSQL 16+; COALESCE over a missing row
		// keeps this working on 15 and earlier rather than failing the startup
		// check for a setting that did not exist yet.
		err := db.QueryRowContext(ctx, `
			SELECT
			  (SELECT setting::int FROM pg_settings WHERE name = 'max_connections'),
			  COALESCE((SELECT setting::int FROM pg_settings WHERE name = 'superuser_reserved_connections'), 0),
			  COALESCE((SELECT setting::int FROM pg_settings WHERE name = 'reserved_connections'), 0)
		`).Scan(&maxConns, &suReserved, &reserved)
		if err != nil {
			return serverConnectionLimit{}, false, fmt.Sprintf("could not read max_connections: %v", err)
		}
		return serverConnectionLimit{
			Max:      maxConns,
			Reserved: suReserved + reserved,
			Detail: fmt.Sprintf("max_connections=%d superuser_reserved=%d reserved=%d",
				maxConns, suReserved, reserved),
		}, true, ""

	case "mysql":
		var maxConns int
		if err := db.QueryRowContext(ctx, `SELECT @@max_connections`).Scan(&maxConns); err != nil {
			return serverConnectionLimit{}, false, fmt.Sprintf("could not read @@max_connections: %v", err)
		}
		// MySQL reserves one slot for a SUPER-privileged connection.
		return serverConnectionLimit{
			Max: maxConns, Reserved: 1,
			Detail: fmt.Sprintf("@@max_connections=%d, one slot reserved for SUPER", maxConns),
		}, true, ""

	default:
		// SQL Server, and anything else. Not an error: the ceiling is 32767 and
		// no cleat deployment approaches it, so there is nothing to warn about.
		// Saying so is better than silence, which reads as "checked and fine".
		return serverConnectionLimit{}, false,
			"not checked on this dialect: the connection ceiling is not a binding constraint here"
	}
}

// assessConnectionFit says whether this worker fits, and whether a second would.
//
// Returns a severity and a message, or an empty severity when there is nothing
// to say. Pure, so the arithmetic is testable without a database -- which is
// most of what can go wrong here.
//
// IT WARNS RATHER THAN REFUSES, unlike checkConnectionBudget. That check is
// about a CONFIGURED budget the operator stated, where a contradiction is
// unambiguously their error. This is about the server's ceiling, where a worker
// that does not fit alongside a second one may be the only worker there is --
// and refusing to start a lone worker because a hypothetical second would not
// fit would be worse than the problem.
func assessConnectionFit(fixed int, limit serverConnectionLimit) (severity, message string) {
	usable := limit.Usable()
	if usable <= 0 {
		return "", ""
	}
	switch {
	case fixed > usable:
		return "error", fmt.Sprintf(
			"this worker's pools may open %d connections and the server allows %d (%s). "+
				"It cannot reach full load. Lower --concurrency, --max-plugin-connections "+
				"or --batch-flush-max-connections, raise the server's limit, or put a "+
				"transaction-mode pooler in front",
			fixed, usable, limit.Detail)
	case 2*fixed > usable:
		return "warn", fmt.Sprintf(
			"this worker's pools may open %d of the %d connections the server allows (%s), "+
				"so a SECOND worker will not fit. Raise the server's limit, or put a "+
				"transaction-mode pooler in front -- see docs/operations/postgresql-sizing.md. "+
				"Note a pooler consolidates these fixed pools but NOT per-tenant pools under "+
				"--tenant-isolation=role, which connect as distinct roles",
			fixed, usable, limit.Detail)
	default:
		return "", ""
	}
}
