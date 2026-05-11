// Command cleatctl is a CLI tool for managing Cleat workflow versions,
// deployments, and operational tasks. It communicates with PostgreSQL
// directly (or via the cleat-worker HTTP API for certain operations).
//
// Usage:
//
//	cleatctl [--db <postgres-dsn>] <command> [<args>]
//
// Commands:
//
//	versions list [<name>]          — list workflow versions
//	versions deprecate <name> <v>   — mark a version deprecated
//	versions restore <name> <v>     — mark a version active
//	versions purge <name> <v>       — permanently delete a version
//	versions active [<name>]        — show active instance counts by version
//	versions gc [--dry-run]         — run garbage collection on deprecated versions
//	deploy workflow <name> <wasm>    — deploy a new workflow WASM binary
//	deploy plugin <name> <wasm>      — deploy a plugin WASM binary
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/rcownie/cleat/internal/host"
)

// osExit is replaced in tests to intercept os.Exit calls.
var osExit = os.Exit

func main() {
	dsn := flag.String("db", "", "PostgreSQL DSN (default: $CLEAT_DB_URL)")
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("CLEAT_DB_URL")
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "error: the --db flag or CLEAT_DB_URL environment variable must be set to a PostgreSQL connection string")
		flag.Usage()
		osExit(1)
	}

	args := flag.Args()
	if len(args) < 1 {
		printUsage()
		osExit(1)
	}

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("failed to ping: %v", err)
	}

	ctx := context.Background()
	factory := host.NewPostgresStoreFactory(db, "public")
	store, closer, err := factory.OpenStore(ctx, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer closer.Close()

	cmd := args[0]
	switch cmd {
	case "versions":
		runVersions(ctx, store, args[1:])
	case "deploy":
		runDeploy(ctx, store, db, args[1:])
	case "cost":
		runCost(args[1:])
	case "restore-workflow":
		runRestoreWorkflow(ctx, store, db, args[1:])
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
  deploy workflow <name> <wasm>   deploy a new workflow WASM binary
  deploy plugin <name> <wasm>     deploy a plugin WASM binary
  cost [flags]                    estimate monthly operational costs
  restore-workflow <id> <file>    restore a single workflow from NDJSON backup

Environment:
  CLEAT_DB_URL   PostgreSQL DSN (alternative to --db)

`)
}
