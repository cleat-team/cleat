// Command cleatctl is a CLI tool for managing Cleat workflow versions,
// deployments, and operational tasks. It communicates with PostgreSQL
// directly (or via the cleat-worker HTTP API for certain operations).
//
// Usage:
//
//	cleatctl [--db <postgres-dsn>] <command> [<args>]
//
// --db must name a PostgreSQL role that row-level security does not apply to
// (a superuser, or one with BYPASSRLS). It is deliberately not the role
// cleat-worker takes: the worker refuses to start on a connection that
// bypasses RLS, and cleatctl needs one. See cleat#1184.
//
// Commands:
//
//	versions list [<name>]          — list workflow versions
//	versions deprecate <name> <v>   — mark a version deprecated
//	versions restore <name> <v>     — mark a version active
//	versions purge <name> <v>       — permanently delete a version
//	versions active [<name>]        — show active instance counts by version
//	versions gc [--dry-run] [--min-versions=N] [--max-age=DURATION]
//	                                — run garbage collection on deprecated versions
//	deploy workflow <name> <wasm>    — deploy a new workflow WASM binary
//	deploy plugin <name> <wasm>      — deploy a plugin WASM binary
//	drop-tenant <tenant-id>          — permanently delete a tenant and all its data
//	suspend-tenant <tenant-id>       — stop new work for a tenant, reversibly
//	resume-tenant <tenant-id>        — undo suspend-tenant
//	revoke-api-key [flags]           — revoke a cleat API key (credential rotation)
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
)

// osExit is replaced in tests to intercept os.Exit calls.
var osExit = os.Exit

// defaultTenantID is the tenant every cleatctl subcommand that does not take a
// tenant argument operates on.
//
// Named rather than repeated because it is now read twice and the two readings
// must agree: the store is opened for this tenant, and check-db locates this
// tenant's per-tenant database (cleat#1956). A second spelling of the literal
// is a second thing to get wrong.
//
// It is also the limit of what those subcommands can see on MySQL, where a
// tenant's tables are in a database of their own -- stated here because the
// hardcoding is easy to read as "tenant-agnostic" when it means the opposite.
const defaultTenantID = "00000000-0000-0000-0000-000000000000"

func main() {
	dsn := flag.String("db", "",
		"database DSN for a role that is a superuser or has BYPASSRLS "+
			"-- NOT the cleat_app role cleat-worker requires (default: $CLEAT_DB_URL). "+
			"PostgreSQL, MySQL and SQL Server are recognised from the DSN's shape")
	driver := flag.String("driver", "",
		"postgres, mysql or mssql. Inferred from --db when unset")
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("CLEAT_DB_URL")
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "error: the --db flag or CLEAT_DB_URL environment variable must be set to a database connection string")
		flag.Usage()
		osExit(1)
	}

	args := flag.Args()
	if len(args) < 1 {
		printUsage()
		osExit(1)
	}

	// The dialect is settled BEFORE the connection is opened, because the
	// driver name is part of opening it -- and before the subcommand runs,
	// because some subcommands are not ported and must refuse rather than
	// fail partway through (see requirePortedFor).
	d := detectDialect(*dsn)
	if *driver != "" {
		var derr error
		if d, derr = dialectByName(*driver); derr != nil {
			log.Fatalf("--driver: %v", derr)
		}
	}

	db, err := sql.Open(d.driver, *dsn)
	if err != nil {
		log.Fatalf("failed to connect to %s database: %v — check the --db flag or CLEAT_DB_URL environment variable", d.name, err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("failed to ping database: %v — check that the database is running and the connection string is correct", err)
	}

	ctx := context.Background()

	// Say once, up front, that this connection cannot answer the questions
	// cleatctl asks. Before the store is opened, so it is the first thing on
	// stderr rather than something to find after a wrong answer. cleat#1184.
	warnIfTenantScoped(ctx, db, d.name)

	factory, err := d.openStoreFactory(db, *dsn, "public")
	if err != nil {
		log.Fatalf("%v", err)
	}
	store, closer, err := factory.OpenStore(ctx, defaultTenantID)
	if err != nil {
		log.Fatalf("failed to open database store: %v — check that the database is accessible and the public schema exists", err)
	}
	defer closer.Close()

	cmd := args[0]

	// Before the subcommand runs: some are not written for this dialect, and
	// the refusal has to precede the first statement rather than follow a
	// partial one.
	requirePortedFor(cmd, d)

	switch cmd {
	case "versions":
		runVersions(ctx, store, args[1:])
	case "deploy":
		runDeploy(ctx, store, db, args[1:])
	case "cost":
		runCost(args[1:])
	case "replay":
		runReplay(ctx, store, db, d, args[1:])
	case "debug":
		runDebug(ctx, store, db, d, args[1:])
	case "check-db":
		runCheckDB(ctx, db, d, *dsn, args[1:])
	case "drop-tenant":
		runDropTenant(ctx, db, d, args[1:])
	case "suspend-tenant":
		runSuspendTenant(ctx, db, d, args[1:], true)
	case "resume-tenant":
		runSuspendTenant(ctx, db, d, args[1:], false)
	case "revoke-api-key":
		runRevokeAPIKey(ctx, db, args[1:])
	case "set-tenant-setting":
		runSetTenantSetting(ctx, db, args[1:])
	case "egress-allow":
		runEgressAllow(ctx, db, d, args[1:])
	case "set-secret":
		runSetSecret(ctx, db, d, args[1:])
	case "retire-secret":
		runRetireSecret(ctx, db, d, args[1:])
	case "queue":
		runQueue(ctx, db, d, *dsn, args[1:])
	case "reseal-payloads":
		runResealPayloads(ctx, db, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		osExit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl [--db <postgres-dsn>] <command> [<args>]

Commands:
  versions list [<name>]          list workflow versions
  versions deprecate <name> <v>   mark a version deprecated
  versions restore <name> <v>     mark a version active
  versions purge <name> <v>       permanently delete a version
  versions active [<name>]        show active instance counts by version
  versions gc [--dry-run]         run garbage collection on deprecated versions
      [--min-versions=N] [--max-age=DURATION]
  deploy workflow <name> <wasm>   deploy a new workflow WASM binary
  deploy plugin <name> <wasm>     deploy a plugin WASM binary
  cost [flags]                    estimate monthly operational costs
  replay <id> --entry-point <n>   replay a workflow's event history for diagnostics
  check-db [--verbose]            verify database connectivity and schema health
  debug <id> [--entry-point <n>] [--watch]  step-through workflow event replay
  drop-tenant <tenant-id> [--dry-run] [--yes]  permanently delete a tenant and all its data
  suspend-tenant <tenant-id> [--yes]           stop new work for a tenant, reversibly
  resume-tenant <tenant-id>                    undo suspend-tenant
  revoke-api-key [--key-id|--key-hash|--key-stdin|--list]  revoke an API key
  egress-allow list <tenant>      show which hosts a tenant's workflows may fetch
  egress-allow add <tenant> <host>...     permit hosts (.example.com = subdomains)
  egress-allow remove <tenant> <host>...  revoke hosts
  queue list <tenant>             show a tenant's declared concurrency queues
  queue create <tenant> <name> --concurrency N  register one, admitting N at a time
  queue disable <tenant> <name>   retire it (its key reverts to a mutex, N=1)
  queue enable <tenant> <name>    put a retired queue back
  reseal-payloads --encryption-key-file <path> [--dry-run]
                                  bind pre-cleat#1776 payload ciphertexts to their tenant

Environment:
  CLEAT_DB_URL   PostgreSQL DSN (alternative to --db)

Which database role:
  cleatctl asks cluster-wide questions, so --db must name a role that
  row-level security does not apply to: a superuser, or one with BYPASSRLS.
  On a connection RLS applies to, commands answer with a single tenant's
  rows and do not say so, and the raw-statement commands fail with
  "cleat.tenant_id is not set".

  This is NOT the role cleat-worker takes. cleat-worker refuses to start on
  a connection that bypasses RLS; cleatctl needs one. Two credentials, on
  purpose -- run cleatctl with the owner DSN you also pass to the worker's
  --migrate-db.

`)
}
