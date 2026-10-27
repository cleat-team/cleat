// Command gen-mssql-baseline builds a core-only SQL Server database from a
// migrations tree, and emits a compacted baseline from a database so built.
//
// It is deliberately a ONE-SHOT TOOL, not a build step -- the same contract
// scripts/gen-postgres-baseline.py carries. The output is three files that
// replace the chain they were generated from, and re-running the tool against
// the compacted tree would fold every post-baseline migration into 001 while
// leaving those files in place to be applied a second time.
//
// Why a Go tool rather than a pg_dump-style text dump: there is no SQL Server
// equivalent of `pg_dump --schema-only` in this tree, and `sys.sql_modules`
// already holds every routine's body verbatim, so the catalogue is a better
// source than a dump. The three emitter traps this runs into -- `sys.objects.type`
// being char(2) and padded, `definition` carrying its leading comments, and
// security policies forcing function-before-policy ordering -- are documented in
// docs/contributor/migrations.md.
//
// cleat#2434.
//
//	# build a core-only database from the chain being compacted
//	go run ./scripts/gen-mssql-baseline -mode=build -dsn "$DSN" -migrations migrations
//
//	# emit the baseline from it
//	go run ./scripts/gen-mssql-baseline -mode=emit -dsn "$DSN" -out /tmp/baseline
//
//	# check the COMMITTED baseline is what the emitter produces (empty database)
//	go run ./scripts/gen-mssql-baseline -mode=verify -dsn "$DSN" \
//	  -committed migrations/mssql
//
// -mode=verify ignores -migrations on purpose: it builds from the three baseline
// files BY NAME, so a later numbered migration cannot be folded into the
// regenerated 001 and fail the check on a correct tree.
//
// Acceptance, in the order it is meant to be run:
//
//	# A: build from the PRE-compaction chain, B: from the shipped files
//	-migrations <chain>          onto A
//	-migrations migrations       onto B
//	-mode=diff         -dsn A -bdsn B     -> must report 0 differences
//	-mode=supplementary -dsn A -bdsn B    -> must report 5 of 5 PASS. Started at
//	                                         11 checks; cleat#2432 moved
//	                                         security policies, triggers,
//	                                         schemas and roles into -mode=diff
//	                                         itself (11 -> 6), cleat#2882 moved
//	                                         "index attributes" the same way,
//	                                         fully subsumed and removed rather
//	                                         than left duplicated (6 -> 5).
//	                                         "column shape" stayed, only
//	                                         partially subsumed -- see its own
//	                                         comment in supplementary.go.
//	scripts/mssql-baseline-known-positive.sh <A-dsn> <B-dsn>
//
// The differential needs the pre-compaction chain as its A side, which after
// the compaction exists only in git history -- so it is a documented operator
// procedure rather than a CI check, exactly as scripts/gen-postgres-baseline.py
// is. -mode=verify is the half that CAN run against the committed tree alone.
//
// Building through cmd/cleat-worker instead would NOT give a core-only
// database. The worker runs core migrations and then plugin.RunMigrations
// unconditionally (cmd/cleat-worker/main.go:1546 and :1580 at develop bd3e7a14),
// plugins record to
// plugin_migrations rather than schema_migrations, and the plugin pass creates
// one `<table>_tenant_isolation` security policy per plugin table -- 25 of them
// here, against core's 14. A database reporting schema_migrations = 77, the
// same as a core-only build, therefore still carries them, so schema_migrations
// is not a scope discriminator. This tool uses migration.NewRunner directly,
// which is the same runner the worker calls at :1527 minus the plugin pass.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/migration/catalogdiff"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen-mssql-baseline:", err)
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", "", "build|emit|diff|supplementary|verify")
	dsn := flag.String("dsn", "", "sqlserver:// DSN, database already created")
	bdsn := flag.String("bdsn", "", "second DSN, for -mode=diff and -mode=supplementary")
	root := flag.String("migrations", "migrations", "migrations root containing mssql/")
	out := flag.String("out", "", "output directory (emit)")
	committed := flag.String("committed", "", "directory holding the committed baseline (verify)")
	flag.Parse()

	switch *mode {
	case "build", "emit", "diff", "supplementary", "verify":
	default:
		return fmt.Errorf("-mode must be build, emit, diff, supplementary or verify, got %q", *mode)
	}
	if *dsn == "" {
		return fmt.Errorf("-dsn is required")
	}

	ctx := context.Background()
	db, err := sql.Open("sqlserver", *dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	switch *mode {
	case "build":
		return build(ctx, db, *root)
	case "diff":
		return diffAgainst(ctx, db, *bdsn)
	case "supplementary":
		return supplementary(ctx, db, *bdsn)
	case "verify":
		if *committed == "" {
			return fmt.Errorf("-committed is required for -mode=verify")
		}
		return verify(ctx, db, *committed)
	default:
		if *out == "" {
			return fmt.Errorf("-out is required for emit")
		}
		return emit(ctx, db, *out)
	}
}

// diffAgainst snapshots both databases and requires an empty structural
// difference. It is the acceptance, not a diagnostic: a non-empty diff is
// returned as an error so a wrapper cannot mistake it for success.
func diffAgainst(ctx context.Context, a *sql.DB, bdsn string) error {
	if bdsn == "" {
		return fmt.Errorf("-bdsn is required for -mode=diff")
	}
	b, err := sql.Open("sqlserver", bdsn)
	if err != nil {
		return err
	}
	defer b.Close()

	catA, err := catalogdiff.Snapshot(ctx, a, migration.DialectMSSQL)
	if err != nil {
		return fmt.Errorf("snapshot A: %w", err)
	}
	catB, err := catalogdiff.Snapshot(ctx, b, migration.DialectMSSQL)
	if err != nil {
		return fmt.Errorf("snapshot B: %w", err)
	}
	diffs := catalogdiff.Diff(catA, catB)
	fmt.Printf("catalogdiff A vs B: %d difference(s)\n", len(diffs))
	for _, d := range diffs {
		fmt.Println("  ", d)
	}
	if len(diffs) != 0 {
		return fmt.Errorf("catalogdiff reported %d difference(s)", len(diffs))
	}
	return nil
}

// build applies every migration in <root>/mssql/ with the real runner, then
// asserts the result is core-only.
//
// The assertion is the point. The default path (cmd/cleat-worker) adds plugin
// migrations silently, so "I built it from the core chain" is not something the
// command line can be trusted to mean -- it has to be checked.
func build(ctx context.Context, db *sql.DB, root string) error {
	r := migration.NewRunner(db, migration.DialectMSSQL, root).
		WithLockTimeout(2 * time.Minute)
	if err := r.Run(ctx); err != nil {
		return fmt.Errorf("run core migrations: %w", err)
	}

	// plugin.RunMigrations creates this; a core-only build never has it.
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sys.tables WHERE name = 'plugin_migrations'`).Scan(&tables); err != nil {
		return fmt.Errorf("probe plugin_migrations: %w", err)
	}
	if tables != 0 {
		return fmt.Errorf("plugin_migrations exists: this database is NOT core-only")
	}

	var applied, policies int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.security_policies`).Scan(&policies); err != nil {
		return fmt.Errorf("count security_policies: %w", err)
	}
	// 14 is core's TenentFilter_* set and 56 is 14 x (1 FILTER + 3 BLOCK); the
	// plugin set would add 25 policies and 75 predicates on top. Asserting the
	// policy count here is what turns "core-only" from a claim into a reading.
	if policies != 14 {
		return fmt.Errorf("expected 14 core security policies, found %d -- plugin policies present?", policies)
	}
	fmt.Printf("core migrations complete: schema_migrations=%d policies=%d plugin_migrations=absent\n",
		applied, policies)
	return nil
}
