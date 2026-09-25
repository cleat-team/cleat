package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// portedOn records, per subcommand, which dialects its SQL has actually been
// written for.
//
// # WHY A REFUSAL AND NOT JUST A DRIVER
//
// cleat#1316 reads as "cleatctl hardcodes the postgres driver", and selecting
// the driver in main.go does make every subcommand CONNECT on all three. That
// is not the same as making them work, and for one of them the difference is
// destructive: drop-tenant issues 17 statements against `admin.*`, and MySQL
// has no `admin` schema -- schema and database are one namespace there, so
// `admin.tenants` is read as a database called `admin` and fails with
// "Error 1049 (42000): Unknown database 'admin'" (auth/tenant_store.go states
// the full matrix).
//
// A drop-tenant that connects and then fails on statement 9 of 17 has half
// deleted a tenant. That is strictly worse than the refusal it replaced, and
// it is the shape cleat's own notes call out: a partial implementation is
// worse than an absent one wherever a caller distinguishes
// present-and-broken from absent.
//
// So the driver is selected for everything, and each subcommand states what it
// has been ported for. The unported ones refuse UP FRONT, naming the dialect
// and what does work, instead of discovering it mid-transaction.
//
// A subcommand absent from this map is unrestricted -- it issues no SQL of its
// own, or goes entirely through engine's store interface, which is already
// dialect-aware.
// THE OTHER HALF OF THIS DECLARATION IS IN THE TEST FILE. Absence from this
// map means "runs on every dialect", so the default for a new subcommand is the
// least conservative behaviour available and it is reached by not typing
// anything -- which is cleat#1956: `queue` was added on 2026-09-20, never
// listed here, and wrote to the wrong physical database on MySQL for two days.
// TestEverySubcommandDeclaresItsDialects now requires every command in
// main.go's dispatch switch to appear either here or in that test's
// unrestrictedSubcommands, with a reason. An absence is no longer a default.
var portedOn = map[string][]string{
	// Ported: the post-incident tools cleat#1316 is about.
	//
	// debug issues no SQL of its own -- it goes through the store interface --
	// so it needed the driver and nothing else. replay carries one statement,
	// whose PostgreSQL casts have explicit arms. check-db is per-dialect
	// throughout, since information_schema does not agree across the three.
	"replay":   {"postgres", "mysql", "mssql"},
	"debug":    {"postgres", "mysql", "mssql"},
	"check-db": {"postgres", "mysql", "mssql"},

	// egress-allow, cleat#1565: written for all three from the start. Its
	// statements are plugin.Query values with a MySQL arm for the unprefixed
	// table, routed through plugin.Rebind -- which is exactly what the
	// unported ones below are missing. TestEgressAllowWorksOnEveryDialect is
	// the test this entry is supposed to have behind it.
	"egress-allow": {"postgres", "mysql", "mssql"},

	// reseal-payloads, cleat#1794: PostgreSQL only, and that is the FEATURE's
	// scope rather than this command's. Encryption at rest is refused unless
	// --driver=postgres (cmd/cleat-worker/main.go:742), the encryptor is
	// attached behind a type assertion to *engine.PostgresStoreFactory, and
	// the encrypting write path's INSERT is Postgres syntax. The mysql and
	// mssql stores' decrypt blocks say so themselves: "encryption is not yet
	// supported and will never be true". So there are no legacy ciphertexts to
	// re-seal on the other two, and a port would be a sweep over nothing.
	"reseal-payloads": {"postgres"},

	// reseal-secrets, cleat#1991: all three, from the start, because tenant
	// secrets exist on all three and a rotation that worked on one would leave the
	// others with no way to retire a key. It reads tenant by tenant under each
	// tenant's own context, so it needs neither a BYPASSRLS role nor a SQL Server
	// admin login (cleat#2123 records why an unscoped read cannot see the table).
	"reseal-secrets": {"postgres", "mysql", "mssql"},

	// reseal-deployment-secrets, cleat#1992 part 1: all three, same reasoning
	// as reseal-secrets -- deployment_secrets exists on all three and carries
	// no tenant dimension to route a read around (engine/deployment_secrets.go).
	"reseal-deployment-secrets": {"postgres", "mysql", "mssql"},

	// Not ported. These carry unqualified `admin.` SQL, which is correct on
	// PostgreSQL and SQL Server and wrong on MySQL, plus $N placeholders that
	// have not been routed through plugin.Rebind.
	//
	// They are listed with the dialects they are KNOWN to work on rather than
	// omitted, so that adding a dialect here is a deliberate act with a test
	// behind it, and so the refusal message can say what does work.
	// drop-tenant gained SQL Server in cleat#1635, with
	// migrations/mssql/074_a_dropped_tenants_rows_go_with_it.sql defining
	// admin.drop_tenant there and TestATenantCanBeDroppedOnSQLServer behind it.
	// MySQL still has no `admin` schema, which is the reason this map exists.
	"drop-tenant":    {"postgres", "mssql"},
	"revoke-api-key": {"postgres"},

	// suspend-tenant / resume-tenant, cleat#1956's audit. These were absent
	// from this map -- so unrestricted -- and on MySQL they do not fail
	// halfway, they fail at the first statement with
	//
	//	error: Error 1049 (42000): Unknown database 'admin'
	//
	// which is the SAME error this file's test already cites as the reason
	// drop-tenant is refused on MySQL. The error is loud, so nothing was
	// silently wrong; what was missing is the refusal that names what does
	// work, which is the whole point of listing a command here.
	//
	// mssql is claimed rather than dropped, and the distinction matters: this
	// change must REFUSE MySQL, not retire a dialect that works. Both
	// statements go through d.rebind, so placeholders are not the problem, and
	// admin.tenants carries `suspended` on SQL Server since
	// migrations/mssql/001_schema.sql:92. Stated as the evidence it is: there
	// is no SQL Server integration test for this command, here or anywhere, so
	// this entry preserves today's behaviour on mssql rather than certifying
	// it. Narrowing it to {"postgres"} would have been the cautious-looking
	// move and would have removed a working path on the strength of not having
	// looked.
	"suspend-tenant":     {"postgres", "mssql"},
	"resume-tenant":      {"postgres", "mssql"},
	"set-tenant-setting": {"postgres"},
	"deploy":             {"postgres"},
	"versions":           {"postgres"},

	// slack, cleat#2230: all three, from the start. slack_workspace carries
	// no admin.-qualified SQL of its own (unlike drop-tenant) and no
	// row-level security to route a connection around (unlike quota) --
	// migrations/postgres/104's own header explains why: the interactive
	// callback resolves a tenant FROM team_id, so there is no tenant
	// already in context to scope a policy by. Every statement here is
	// $N-shaped and rewritten through d.rebind, the same convention
	// TestSlackWorkspaceStatementsRebindPerDialect pins for quota's own
	// statements.
	"slack": {"postgres", "mysql", "mssql"},

	// backup, cleat#2247: all three, from the start. backup_config and
	// backup_history carry no admin.-qualified SQL (unlike drop-tenant) and
	// no row-level security to route a connection around, since migration
	// v4 dropped both tables' tenant_id column and RLS policy on every
	// dialect. Every statement is $N-shaped and rewritten through
	// d.rebind, the same convention slack's own entry above states --
	// except backup history's LIMIT, which is not valid T-SQL and needed
	// its own MSSQL arm (backupHistoryListSQL/backupHistoryListByConfigSQL
	// are plugin.Query, not plain strings, for exactly that reason).
	//
	// Verified empirically, not by resemblance to slack:
	// TestBackupCommandWorksOnEveryDialect drives config-create,
	// config-update, run, history and config-delete against real
	// PostgreSQL, MySQL and SQL Server containers. It found three real
	// defects before this entry was written: MySQL's `?` binds by
	// APPEARANCE rather than by number, so a $N reused for two columns
	// (created_at/updated_at, then next_run_at/updated_at) silently needed
	// a second argument; resolveConfigID scanned a *uuid.UUID directly
	// instead of through plugin.ScanRow, so SQL Server's mixed-endian
	// UNIQUEIDENTIFIER bytes produced a wrong id the moment one was read
	// back and reused (cleat#1137); and `backup history`'s LIMIT did not
	// run on SQL Server AT ALL ("Incorrect syntax near 'LIMIT'").
	"backup": {"postgres", "mysql", "mssql"},
}

// requirePortedFor exits with a clear message when cmd has not been written for
// this dialect.
//
// It is called BEFORE the subcommand runs and before any statement is issued,
// which is the whole point: the failure it replaces is a partial one.
func requirePortedFor(cmd string, d dialect) {
	supported, restricted := portedOn[cmd]
	if !restricted {
		return
	}
	for _, s := range supported {
		if s == d.name {
			return
		}
	}
	sorted := append([]string(nil), supported...)
	sort.Strings(sorted)
	fmt.Fprintf(os.Stderr,
		"error: `cleatctl %s` has not been ported to %s.\n\n"+
			"It is written for: %s.\n\n"+
			"This is a refusal rather than an attempt because %s issues SQL that is\n"+
			"not portable as written, and failing partway through would leave the\n"+
			"database in a state no one asked for. `replay`, `debug` and `check-db`\n"+
			"do work on %s -- those are the post-incident tools (cleat#1316).\n",
		cmd, d.name, strings.Join(sorted, ", "), cmd, d.name)
	osExit(1)
}
