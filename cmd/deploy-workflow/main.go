// Command deploy-workflow deploys a workflow WASM binary to a cleat database.
// Supports postgres, mysql, and mssql backends. Used by e2e_test.sh.
//
// Build: go build -o deploy-workflow ./test/deploy_workflow.go
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	dbURL := flag.String("db", "", "Database connection URL")
	driver := flag.String("driver", "postgres", "Database driver (postgres, mysql, mssql)")
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: deploy-workflow [flags] <name> <wasm-file>")
		os.Exit(1)
	}
	if *dbURL == "" {
		*dbURL = os.Getenv("CLEAT_DB_URL")
	}
	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "error: --db or CLEAT_DB_URL required")
		os.Exit(1)
	}

	name := args[0]
	wasmPath := args[1]

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", wasmPath, err)
		os.Exit(1)
	}
	if len(wasmBytes) == 0 {
		fmt.Fprintln(os.Stderr, "error: empty WASM file")
		os.Exit(1)
	}
	hash := sha256.Sum256(wasmBytes)

	ctx := context.Background()
	var store engine.WorkflowStore
	var closer io.Closer

	switch *driver {
	case "postgres":
		db, err := sql.Open("postgres", *dbURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open postgres: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		factory := engine.NewPostgresStoreFactory(db, "public")
		store, closer, err = factory.OpenStore(ctx, "00000000-0000-0000-0000-000000000000", "default")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open store: %v\n", err)
			os.Exit(1)
		}
		defer closer.Close()

	case "mysql":
		db, err := sql.Open("mysql", *dbURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open mysql: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		baseDSN := mysqlBaseDSN(*dbURL)
		factory := engine.NewMySQLStoreFactory(db, baseDSN)
		store, closer, err = factory.OpenStore(ctx, "00000000-0000-0000-0000-000000000000", "default")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open store: %v\n", err)
			os.Exit(1)
		}
		defer closer.Close()

	case "mssql":
		factory := engine.NewMSSQLStoreFactory(*dbURL)
		store, closer, err = factory.OpenStore(ctx, "00000000-0000-0000-0000-000000000000", "default")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open store: %v\n", err)
			os.Exit(1)
		}
		defer closer.Close()

	default:
		fmt.Fprintf(os.Stderr, "unsupported driver: %s\n", *driver)
		os.Exit(1)
	}

	// The version embedded in the WASM wins, and auto-increment is only the
	// fallback when there is none.
	//
	// This used to auto-increment unconditionally. The engine validates the
	// deployed row's version against the binary's own metadata at run time
	// (error_op "version_check"), so a redeploy of changed bytes wrote
	// def_version 2 for a binary still reporting 1, and every run of that
	// workflow then failed with:
	//
	//	version mismatch: workflow instance <id> expects def_version 2 but
	//	WASM binary metadata reports version 1 (def=<name>). The
	//	workflow_defs row and the deployed WASM binary are out of sync.
	//
	// Not a corner: this is the only multi-dialect deploy path cleat has --
	// cmd/cleat/db.go's openPostgresDB refuses a MySQL or SQL Server DSN and
	// names this binary as the alternative -- so on those two dialects the
	// SECOND deploy of any workflow produced one that could not run. Found by
	// pointing the port harness at this binary and re-running.
	//
	// cmd/cleat's deploy has always read the metadata first and falls back the
	// same way; this makes the two agree.
	version := 0
	if meta, metaErr := wasm.ReadMetadata(wasmBytes); metaErr == nil && meta.WorkflowVersion > 0 {
		version = meta.WorkflowVersion
	}

	// Determine next version number.
	existingDefs, _ := store.ListWorkflowDefs(ctx, name)
	nextVersion := 1
	for _, def := range existingDefs {
		if def.Version >= nextVersion {
			nextVersion = def.Version + 1
		}
		if len(def.WASMBytes) > 0 {
			existingHash := sha256.Sum256(def.WASMBytes)
			if existingHash == hash {
				fmt.Printf("WASM unchanged: %s v%d (skipped)\n", name, def.Version)
				return
			}
		}
	}
	version = chooseDeployVersion(version, nextVersion)
	minVersion := version - 1
	if minVersion < 1 {
		minVersion = 1
	}

	def := &engine.WorkflowDef{
		Name:       name,
		Version:    version,
		WASMBytes:  wasmBytes,
		ABIVersion: 1,
		MinVersion: minVersion,
		CreatedAt:  time.Now(),
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		fmt.Fprintf(os.Stderr, "error deploying %s v%d: %v\n", name, version, err)
		os.Exit(1)
	}
	fmt.Printf("Deployed %s v%d (%d bytes, SHA256=%x) to %s\n",
		name, version, len(wasmBytes), hash[:8], *driver)
}

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

// chooseDeployVersion picks the version a deploy writes: the one embedded in
// the WASM when there is one, otherwise the next free number.
//
// A function so it can be tested. The rule is one line, and it was still worth
// extracting -- getting it wrong does not fail the deploy, it produces a
// workflow_defs row the engine rejects at RUN time, on a code path only the
// second deploy of a given workflow reaches.
func chooseDeployVersion(embedded, next int) int {
	if embedded > 0 {
		return embedded
	}
	return next
}
