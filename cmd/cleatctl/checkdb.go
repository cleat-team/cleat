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

	// 3. Table accessibility.
	tables := []string{
		"workflow_instances",
		"event_history",
		"workflow_defs",
		"workflow_signals",
		"workflow_promises",
		"child_workflows",
		"workflow_schedules",
		"workflow_dead_letters",
		"idempotency_keys",
		"concurrency_keys",
		"tenant_api_keys",
		"plugin_registry",
		"plugin_audit_log",
	}
	var missingTables []string
	var accessibleCount int
	for _, table := range tables {
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1 AND table_schema NOT IN ('pg_catalog', 'information_schema')",
			table,
		).Scan(&count)
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
	} else if verbose {
		fmt.Fprintf(os.Stderr, "INSTANCES: WARNING: cannot query workflow_instances: %v\n", err)
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
	} else if verbose {
		// Fallback: count rows
		var rowCount int64
		if countErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_history").Scan(&rowCount); countErr == nil {
			fmt.Printf("EVENT HISTORY: %d rows\n", rowCount)
		}
	}

	// 6. Dead letter queue count.
	var deadLetterCount int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_dead_letters").Scan(&deadLetterCount)
	if err == nil && deadLetterCount > 0 && verbose {
		fmt.Printf("DEAD LETTERS: %d workflows\n", deadLetterCount)
	}

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
