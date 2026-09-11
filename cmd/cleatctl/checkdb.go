package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// check-db command
// ---------------------------------------------------------------------------

// runCheckDB verifies database connectivity and schema health.
// It connects to the database, pings it, checks the schema migration version,
// inspects workflow instance counts, and reports overall health status.
func runCheckDB(ctx context.Context, db *sql.DB, args []string) {
	verbose := false
	for _, arg := range args {
		if arg == "--verbose" || arg == "-v" {
			verbose = true
		}
	}

	// issues is the single source of truth for health. There used to be a
	// parallel `healthy` bool as well, and the two had already drifted: every
	// check after the ping keys off len(issues), so the `healthy = false` in
	// the accessible-tables check (found by ineffassign) was never read. That
	// happened to be harmless, because the same branch also appends to issues
	// -- but a check added later that set only the bool would have failed to
	// fail, silently, which is the worst way for a health check to be wrong.
	issues := []string{}

	// 1. Ping.
	pingStart := time.Now()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "DATABASE: DISCONNECTED (%v)\n", err)
		issues = append(issues, fmt.Sprintf("ping failed: %v", err))
	} else {
		pingDur := time.Since(pingStart)
		fmt.Printf("DATABASE: connected (ping: %v)\n", pingDur)
	}

	// A failed ping is fatal on its own: every check below needs the
	// connection, so continuing would report a cascade of derived failures.
	if len(issues) > 0 {
		fmt.Fprintf(os.Stderr, "\nSTATUS: UNHEALTHY\n")
		fmt.Fprintf(os.Stderr, "Issues:\n")
		for _, issue := range issues {
			fmt.Fprintf(os.Stderr, "  - %s\n", issue)
		}
		osExit(1)
	}

	// 2. Schema version.
	var schemaVersion string
	var appliedAt *time.Time
	err := db.QueryRowContext(ctx, `
		SELECT version, applied_at
		FROM schema_migrations
		ORDER BY version DESC
		LIMIT 1
	`).Scan(&schemaVersion, &appliedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		fmt.Fprintf(os.Stderr, "SCHEMA: WARNING: cannot read schema version: %v\n", err)
		issues = append(issues, fmt.Sprintf("schema version check failed: %v", err))
	} else if errors.Is(err, sql.ErrNoRows) {
		fmt.Println("SCHEMA: no migrations applied yet (empty or fresh database)")
		if verbose {
			fmt.Println("  version: (none)")
		}
	} else {
		timeStr := ""
		if appliedAt != nil {
			timeStr = appliedAt.Format(time.RFC3339)
		}
		fmt.Printf("SCHEMA: version %s (applied: %s)\n", schemaVersion, timeStr)
	}

	// 2b. What this connection can see.
	//
	// Reported before the table checks because it EXPLAINS them: on a
	// connection row-level security applies to, the reads below fail or
	// return one tenant's rows, and without this line the operator is left to
	// infer a permissions problem from a count. Telling them what their
	// database looks like is this command's whole job. cleat#1184.
	switch posture, reasons, rerr := rlsPostureFn(ctx, db); {
	case rerr != nil:
		fmt.Fprintf(os.Stderr, "RLS: WARNING: cannot determine enforcement: %v\n", rerr)
		issues = append(issues, fmt.Sprintf("row-level security check failed: %v", rerr))
	case posture == rlsExempt:
		fmt.Println("RLS: connection is exempt (superuser or BYPASSRLS) -- reads are cluster-wide")
	case posture == rlsSubject:
		fmt.Fprintln(os.Stderr, "RLS: connection IS subject to row-level security -- reads below "+
			"are scoped to one tenant, or fail")
		issues = append(issues, "--db is subject to row-level security: cleatctl needs a "+
			"superuser or BYPASSRLS role, not the cleat_app role cleat-worker takes")
	case posture == rlsUnprotected:
		// Reads will work, and that is the bad news rather than the good.
		// Nothing is isolating tenants in this database.
		fmt.Fprintln(os.Stderr, "RLS: this DATABASE is not enforcing tenant isolation:")
		for _, r := range reasons {
			fmt.Fprintf(os.Stderr, "  - %s\n", r.Detail)
		}
		issues = append(issues, "row-level security is not enforced by this database")
	}

	// 3. Table accessibility.
	//
	// coreTables is every table migrations/postgres/ creates, and it is checked
	// against those files by TestCoreTablesMatchTheMigrations. Before that test
	// existed this was a hand-maintained list that had drifted in BOTH
	// directions, silently, for as long as anyone had run the command:
	//
	//   - FOUR names that no migration has ever created --  child_workflows,
	//     workflow_dead_letters, plugin_registry, plugin_audit_log -- so every
	//     healthy database was told "TABLES: 9 accessible, 4 missing";
	//   - and TEN real core tables it never looked at, including
	//     workflow_routing, workflow_tags and workflow_update_requests.
	//
	// The over-reporting is what got noticed, because it prints. The
	// under-reporting is the worse half: a command whose job is to say whether
	// the schema is complete was answering about a subset chosen by hand.
	//
	// Two of the four phantoms were not renames and are not coming back.
	// child_workflows: parent/child is workflow_instances.parent_workflow_id, a
	// column. plugin_audit_log: the only audit-shaped table is audit_events,
	// created by plugins/auditlog -- a PLUGIN, present only where it is
	// installed, which a core health check must not require. cleat#1216.
	tables := coreTables
	var missingTables []string
	var accessibleCount int
	for _, table := range tables {
		// Schema-qualified names are matched on BOTH parts. Matching on
		// table_name alone -- which is what this did -- makes `admin.tenants`
		// and a `tenants` in any other schema indistinguishable, so a table in
		// the wrong schema reads as present. That mattered the moment the list
		// stopped being purely public: four of these live in `admin`.
		schemaName, bareName, qualified := strings.Cut(table, ".")
		if !qualified {
			bareName = table
		}
		var count int
		var err error
		if qualified {
			err = db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2",
				schemaName, bareName,
			).Scan(&count)
		} else {
			err = db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1 AND table_schema NOT IN ('pg_catalog', 'information_schema')",
				bareName,
			).Scan(&count)
		}
		if err != nil {
			// information_schema might not exist on all drivers; try a simple count instead.
			var rowCount int64
			checkErr := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&rowCount)
			if checkErr != nil {
				missingTables = append(missingTables, table)
				continue
			}
			accessibleCount++
		} else if count > 0 {
			accessibleCount++
		} else {
			missingTables = append(missingTables, table)
		}
	}
	if len(missingTables) > 0 {
		fmt.Printf("TABLES: %d accessible, %d missing\n", accessibleCount, len(missingTables))
		if verbose {
			for _, t := range missingTables {
				fmt.Printf("  MISSING: %s\n", t)
			}
		}
		if accessibleCount == 0 {
			issues = append(issues, "no accessible tables (schema mismatch or wrong database)")
		}
	} else {
		fmt.Printf("TABLES: all %d accessible\n", accessibleCount)
	}

	// 4. Workflow instance counts by status.
	type statusCount struct {
		Status string
		Count  int64
	}
	rows, err := db.QueryContext(ctx, `
		SELECT status, COUNT(*) AS cnt
		FROM workflow_instances
		GROUP BY status
		ORDER BY status
	`)
	if err == nil {
		defer rows.Close()
		var totalInstances int64
		var statusCounts []statusCount
		for rows.Next() {
			var sc statusCount
			if err := rows.Scan(&sc.Status, &sc.Count); err != nil {
				continue
			}
			statusCounts = append(statusCounts, sc)
			totalInstances += sc.Count
		}
		fmt.Printf("INSTANCES: %d total\n", totalInstances)
		if verbose && len(statusCounts) > 0 {
			var parts []string
			for _, sc := range statusCounts {
				parts = append(parts, fmt.Sprintf("%s: %d", sc.Status, sc.Count))
			}
			fmt.Printf("  by status: %s\n", strings.Join(parts, ", "))
		}
	} else {
		// Not `else if verbose`, and that is the whole point of cleat#1184.
		// This branch used to print nothing without --verbose, so the section
		// simply vanished from the report and STATUS stayed "healthy" -- a
		// diagnostic command reporting health about a table it could not read.
		// Measured on a cleat_app connection: exit 0, "STATUS: healthy", and
		// the INSTANCES and EVENT HISTORY lines absent with no trace.
		fmt.Fprintf(os.Stderr, "INSTANCES: UNREADABLE: %v\n", err)
		issues = append(issues, fmt.Sprintf("cannot read workflow_instances: %v", err))
	}

	// 5. Event history size estimate.
	var histSize int64
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(pg_column_size(row_to_json(event_history.*))), 0)
		FROM event_history
	`).Scan(&histSize)
	if err == nil {
		sizeMB := float64(histSize) / (1024 * 1024)
		fmt.Printf("EVENT HISTORY: %.1f MB\n", sizeMB)
	} else {
		// The row-count fallback first: pg_column_size over row_to_json is the
		// expensive form and can fail where a plain COUNT(*) succeeds. Only
		// when BOTH fail has the table proved unreadable.
		var rowCount int64
		if countErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_history").Scan(&rowCount); countErr == nil {
			fmt.Printf("EVENT HISTORY: %d rows\n", rowCount)
		} else {
			fmt.Fprintf(os.Stderr, "EVENT HISTORY: UNREADABLE: %v\n", countErr)
			issues = append(issues, fmt.Sprintf("cannot read event_history: %v", countErr))
		}
	}

	// 6. Dead letters are reported by section 4 above, and were never reported
	// here.
	//
	// This section used to run `SELECT COUNT(*) FROM workflow_dead_letters`
	// behind `if err == nil && deadLetterCount > 0 && verbose`. There is no
	// such table -- dead_lettered is a STATUS on workflow_instances
	// (migrations/postgres/033, 052) -- so the query always errored, err was
	// never nil, and the DEAD LETTERS line has never printed once. Removing it
	// cannot regress output that was never produced.
	//
	// Not repointed at workflow_instances, because section 4's `by status`
	// line already carries the number under the same --verbose gate: on a
	// database with five, it prints `dead_lettered: 5`. A second statement for
	// a figure already on screen is a second thing to drift, which is the
	// defect this file is being repaired for. cleat#1216.

	// 7. Summary.
	if len(issues) > 0 {
		fmt.Fprintf(os.Stderr, "\nSTATUS: DEGRADED\n")
		for _, issue := range issues {
			fmt.Fprintf(os.Stderr, "  - %s\n", issue)
		}
		osExit(1)
	}

	// Output structured JSON for programmatic consumption if verbose.
	if verbose {
		summary := map[string]any{
			"status":            "healthy",
			"tables_accessible": accessibleCount,
			"total_tables":      len(tables),
		}
		jsonSummary, _ := json.MarshalIndent(summary, "", "  ")
		fmt.Fprintf(os.Stderr, "\nJSON summary:\n%s\n", string(jsonSummary))
	}

	fmt.Println("\nSTATUS: healthy")
}

// printCheckDBUsage prints the help text for check-db.
func printCheckDBUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl check-db [--verbose]

Verify database connectivity and schema health.

Checks performed:
  - Database ping (connectivity)
  - Schema migration version
  - Table accessibility (all required tables exist)
  - Workflow instance counts by status
  - Event history size estimate

Flags:
  --verbose, -v  Include detailed output and JSON summary

Environment:
  CLEAT_DB_URL   PostgreSQL DSN (alternative to --db)

`)
}
