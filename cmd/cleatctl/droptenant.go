package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// drop-tenant command
// ---------------------------------------------------------------------------
//
// Surface choice, and why it is here and not an HTTP endpoint (Finding S3,
// item 2). cleat-worker's admin API (cmd/cleat-worker/api_admin.go) is
// gated on callerOwnsTarget: the caller's own tenant-scoped API key proves
// they own the *target* of the operation. That check has no meaning here --
// deleting an entire tenant is a platform-operator action, not something
// any tenant (including the one being deleted) should ever be able to
// trigger against itself or another tenant, and there is no
// platform-operator identity anywhere in this codebase's HTTP auth today
// (auth/tenant_store.go resolves a request to exactly one tenant; nothing
// resolves "the operator running the deployment"). Building that identity
// layer just to hang this one destructive command off it is a separate,
// much bigger piece of work than Finding S3, so this command instead joins
// check-db, restore-workflow, and versions purge/gc as a DBA-only
// operation: it authenticates the same way they do, by requiring a
// PostgreSQL connection string with sufficient privilege (--db /
// CLEAT_DB_URL), not a tenant API key. Whoever can run cleatctl against the
// production database can already do more damage than this command does
// (versions purge, deploy) with the same access.
//
// Guard rails (Finding S3, item 3):
//   - Always counts and prints every row it is about to delete before
//     asking for confirmation -- a dry-run is not a separate mode, it is
//     what every invocation does first.
//   - --dry-run stops there and deletes nothing.
//   - Otherwise, requires the operator to type the tenant ID back exactly
//     (not a y/N, which is too easy to reflexively type for an operation
//     this destructive) unless --yes is passed for scripted use.
//   - Refuses the default tenant outright, matching the guard
//     migrations/postgres/032_drop_tenant_deletes_tenant_data.sql adds to
//     admin.drop_tenant itself -- checked here too so the operator gets a
//     clear error before a confirmation prompt, not just relies on the SQL
//     guard firing.
//   - Prints the pre-deletion counts again after a successful delete, as
//     the audit record: this command has no dedicated audit table (a
//     bigger schema change than Finding S3's scope), so the printed output
//     -- which an operator invoking a destructive DBA tool is expected to
//     be capturing in their own shell history / session log / ops runbook
//     already -- is what stands in for one. Documented here rather than
//     assumed.

// dropTenantTableCounts mirrors the tables admin.drop_tenant deletes, for
// the dry-run / confirmation / audit output. Keep this list in sync with
// migrations/postgres/032_drop_tenant_deletes_tenant_data.sql.
var dropTenantTables = []struct {
	label string
	query string
}{
	{"workflow_instances", `SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`},
	{"event_history", `SELECT count(*) FROM event_history WHERE tenant_id = $1`},
	{"workflow_signals", `SELECT count(*) FROM workflow_signals WHERE tenant_id = $1`},
	{"workflow_promises", `SELECT count(*) FROM workflow_promises WHERE tenant_id = $1`},
	{"concurrency_keys", `SELECT count(*) FROM concurrency_keys WHERE tenant_id = $1`},
	{"workflow_update_requests", `SELECT count(*) FROM workflow_update_requests WHERE tenant_id = $1`},
	{"workflow_schedules", `SELECT count(*) FROM workflow_schedules WHERE tenant_id = $1`},
	{"workflow_tags", `SELECT count(*) FROM workflow_tags WHERE tenant_id = $1`},
	{"workflow_routing", `SELECT count(*) FROM workflow_routing WHERE tenant_id = $1`},
	{"idempotency_keys", `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`},
	{"admin.tenant_api_keys", `SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = $1`},
	{"admin.tenant_roles", `SELECT count(*) FROM admin.tenant_roles WHERE tenant_id = $1`},
	{"admin.tenants", `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`},
}

// countTenantRows returns the row count for each table in dropTenantTables,
// in order, plus the total.
func countTenantRows(ctx context.Context, db *sql.DB, tenantID string) ([]int64, int64, error) {
	counts := make([]int64, len(dropTenantTables))
	var total int64
	for i, tbl := range dropTenantTables {
		var n int64
		if err := db.QueryRowContext(ctx, tbl.query, tenantID).Scan(&n); err != nil {
			return nil, 0, fmt.Errorf("count %s: %w", tbl.label, err)
		}
		counts[i] = n
		total += n
	}
	return counts, total, nil
}

func printTenantRowCounts(w *tabwriter.Writer, counts []int64, total int64) {
	for i, tbl := range dropTenantTables {
		fmt.Fprintf(w, "  %s\t%d\n", tbl.label, counts[i])
	}
	w.Flush()
	fmt.Printf("  TOTAL\t%d\n", total)
}

func runDropTenant(ctx context.Context, db *sql.DB, args []string) {
	var tenantID string
	dryRun := false
	yes := false
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--yes":
			yes = true
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
				printDropTenantUsage()
				osExit(1)
			}
			if tenantID != "" {
				fmt.Fprintf(os.Stderr, "unexpected extra argument: %s\n", a)
				printDropTenantUsage()
				osExit(1)
			}
			tenantID = a
		}
	}
	if tenantID == "" {
		printDropTenantUsage()
		osExit(1)
	}

	// Mirrors the guard admin.drop_tenant itself now enforces
	// (migrations/postgres/032_drop_tenant_deletes_tenant_data.sql) --
	// checked here too so the operator gets a clear, specific error before
	// any confirmation prompt, rather than the SQL function's exception
	// surfacing after they have already typed a confirmation.
	if tenantID == engine.DefaultTenantUUID {
		fmt.Fprintf(os.Stderr, "error: refusing to drop the default tenant (%s) -- it is shared by "+
			"every single-tenant deployment and by workflow_defs/plugin_defs, which are not "+
			"tenant-owned data\n", engine.DefaultTenantUUID)
		osExit(1)
	}

	var tenantName string
	if err := db.QueryRowContext(ctx, `SELECT name FROM admin.tenants WHERE tenant_id = $1`, tenantID).Scan(&tenantName); err != nil {
		if err == sql.ErrNoRows {
			tenantName = "(no admin.tenants row for this ID)"
		} else {
			fmt.Fprintf(os.Stderr, "error looking up tenant name: %v\n", err)
			osExit(1)
		}
	}

	counts, total, err := countTenantRows(ctx, db, tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error counting tenant data: %v\n", err)
		osExit(1)
	}

	fmt.Printf("Tenant: %s (%s)\n", tenantID, tenantName)
	fmt.Println("Rows that would be permanently deleted:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	printTenantRowCounts(w, counts, total)

	if total == 0 {
		fmt.Println("\nNothing to delete.")
		return
	}

	if dryRun {
		fmt.Println("\n--dry-run: nothing deleted.")
		return
	}

	if !yes {
		fmt.Printf("\nThis permanently deletes ALL %d rows above for tenant %s (%s).\n", total, tenantID, tenantName)
		fmt.Printf("Type the tenant ID to confirm: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != tenantID {
			fmt.Println("confirmation did not match -- cancelled")
			return
		}
	}

	if _, err := db.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, tenantID); err != nil {
		fmt.Fprintf(os.Stderr, "error dropping tenant: %v\n", err)
		osExit(1)
	}

	// Audit record: this command has no dedicated audit table (see the
	// package doc comment above), so this printed summary -- the same
	// counts gathered before deletion, since every count is now zero -- is
	// the record of what was deleted.
	fmt.Printf("\nDeleted tenant %s (%s). Rows removed:\n", tenantID, tenantName)
	w2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	printTenantRowCounts(w2, counts, total)
}

func printDropTenantUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl drop-tenant <tenant-id> [--dry-run] [--yes]

Permanently delete a tenant and every row of its data: workflow_instances,
event_history, workflow_signals, workflow_promises, concurrency_keys,
workflow_update_requests, workflow_schedules, workflow_tags,
workflow_routing, idempotency_keys, admin.tenant_api_keys,
admin.tenant_roles, admin.tenants, plus the tenant's plugin schema and
Postgres role. Does not touch workflow_defs/plugin_defs (shared registry,
not tenant-owned data). Refuses the default tenant
(00000000-0000-0000-0000-000000000000).

Always prints a full row count for every affected table before doing
anything else, whether or not --dry-run is given.

Flags:
  --dry-run   count and print what would be deleted; delete nothing
  --yes       skip the interactive "type the tenant ID to confirm" prompt
              (for scripted use -- still requires the tenant ID argument)

This is a DBA-only operation, authenticated the same way every other
cleatctl command is: by the database connection string (--db /
CLEAT_DB_URL), not a tenant API key. There is no HTTP endpoint for this.

`)
}
