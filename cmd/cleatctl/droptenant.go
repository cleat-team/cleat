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
	"github.com/cleat-team/cleat/internal/tenantdrop"
	"github.com/cleat-team/cleat/plugin"
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
// check-db and versions purge/gc as a DBA-only
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
//     admin.drop_tenant itself carries (migrations/postgres/001_schema.sql)
//     -- checked here too so the operator gets a clear error before a
//     confirmation prompt, not just relies on the SQL guard firing.
//   - Prints the pre-deletion counts again after a successful delete, as
//     the audit record: this command has no dedicated audit table (a
//     bigger schema change than Finding S3's scope), so the printed output
//     -- which an operator invoking a destructive DBA tool is expected to
//     be capturing in their own shell history / session log / ops runbook
//     already -- is what stands in for one. Documented here rather than
//     assumed.

// dropTenantTables is the shared list (internal/tenantdrop.PostgresTables) in this
// command's counting shape. It moved to internal/tenantdrop so the worker can plan a
// drop from the same list; TestThePreviewNamesEveryTenantOwnedTable still pins it,
// through this variable, against the migrations.
var dropTenantTables = func() []struct{ label, query string } {
	out := make([]struct{ label, query string }, len(tenantdrop.PostgresTables))
	for i, t := range tenantdrop.PostgresTables {
		out[i] = struct{ label, query string }{t.Label, t.Query}
	}
	return out
}()

// rowCounter is the subset of *sql.DB and *sql.Conn this command counts
// through.
//
// A *sql.Conn rather than only a *sql.DB because SQL Server's counts are
// meaningless without SESSION_CONTEXT('tenant_id'), which is per-connection
// state -- see droptenant_mssql.go. PostgreSQL passes the pool and is
// unaffected.
type rowCounter interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// countTenantRows returns the row count for each table given, in order, plus
// the total.
func countTenantRows(ctx context.Context, q rowCounter, tables []struct{ label, query string }, tenantID string) ([]int64, int64, error) {
	counts := make([]int64, len(tables))
	var total int64
	for i, tbl := range tables {
		var n int64
		if err := q.QueryRowContext(ctx, tbl.query, tenantID).Scan(&n); err != nil {
			return nil, 0, fmt.Errorf("count %s: %w", tbl.label, err)
		}
		counts[i] = n
		total += n
	}
	return counts, total, nil
}

func printTenantRowCounts(w *tabwriter.Writer, tables []struct{ label, query string }, counts []int64, total int64) {
	for i, tbl := range tables {
		fmt.Fprintf(w, "  %s\t%d\n", tbl.label, counts[i])
	}
	w.Flush()
	fmt.Printf("  TOTAL\t%d\n", total)
}

func runDropTenant(ctx context.Context, db *sql.DB, d dialect, args []string) {
	var tenantID string
	dryRun := false
	yes := false
	// Defaults to "public", matching cleat-worker's --schema flag. The value is
	// passed to admin.drop_tenant EXPLICITLY rather than left to search_path;
	// see the call below and cleat#1363.
	schema := "public"
	schemaGiven := false
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dryRun = true
		case a == "--yes":
			yes = true
		case strings.HasPrefix(a, "--schema="):
			schema = strings.TrimPrefix(a, "--schema=")
			schemaGiven = true
			if schema == "" {
				fmt.Fprintln(os.Stderr, "--schema= requires a value")
				printDropTenantUsage()
				osExit(1)
			}
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
	// (migrations/postgres/001_schema.sql) --
	// checked here too so the operator gets a clear, specific error before
	// any confirmation prompt, rather than the SQL function's exception
	// surfacing after they have already typed a confirmation.
	if tenantID == engine.DefaultTenantUUID {
		// Says plugin_defs only. workflow_defs was in this sentence and is
		// tenant-owned -- primary key (tenant_id, name, version) -- which is
		// cleat#1201. The reason to refuse the default tenant is unchanged and
		// does not depend on that: every single-tenant deployment writes under
		// it. admin.drop_tenant's own RAISE EXCEPTION still carries the older
		// wording; correcting it means redefining the function, which 059
		// deliberately did not do.
		fmt.Fprintf(os.Stderr, "error: refusing to drop the default tenant (%s) -- it is shared by "+
			"every single-tenant deployment and by plugin_defs, which is not tenant-owned data\n",
			engine.DefaultTenantUUID)
		osExit(1)
	}

	// Pin search_path to the SAME schema that is passed to admin.drop_tenant
	// below, so the preview the operator confirms describes the tables that are
	// actually about to be deleted.
	//
	// Without this, --schema would change what is DELETED and not what is
	// COUNTED, and the confirmation prompt would show the row counts of a
	// different schema -- an operator approving a number that refers to
	// somewhere else. A preview that does not describe the action is worse than
	// no preview.
	//
	// quote_ident and a bind parameter rather than string interpolation: the
	// statement is static, so it needs no exemption from the inline-SQL parse
	// test, and a schema name needing quotes is handled by the server.
	//
	// PostgreSQL only, along with --schema itself: WithSchema has no effect on
	// any other dialect, so on SQL Server there is exactly one place plugin
	// tables can be and nothing to pin.
	var (
		counter   rowCounter = db
		tables               = dropTenantTables
		mssqlConn *sql.Conn
	)
	switch d.name {
	case "postgres":
		if _, err := db.ExecContext(ctx,
			`SELECT set_config('search_path', quote_ident($1), false)`, schema); err != nil {
			fmt.Fprintf(os.Stderr, "error selecting schema %q: %v\n", schema, err)
			osExit(1)
		}
	case "mssql":
		// Refuse rather than ignore. A flag that is accepted and does nothing
		// is how an operator comes to believe they deleted from a schema this
		// dialect cannot put tables in.
		if schemaGiven {
			fmt.Fprintf(os.Stderr, "error: --schema is PostgreSQL-only -- plugin tables are always in dbo on SQL Server\n")
			osExit(1)
		}
		// Held for the rest of the command: the tenant key below is
		// per-connection, and every count depends on it. See
		// droptenant_mssql.go.
		conn, err := db.Conn(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error opening a connection: %v\n", err)
			osExit(1)
		}
		defer conn.Close()
		if err := setMSSQLTenantKey(ctx, conn, tenantID); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(1)
		}
		tables, err = mssqlTenantTables(ctx, conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(1)
		}
		counter, mssqlConn = conn, conn
	}

	// Through counter, not db: counter is the connection every count here is
	// read on, and on SQL Server that is the pinned one whose tenant key was set
	// above. This comment used to say admin.tenants carries a row policy there.
	// It does not: no security predicate targets any admin.* table (measured
	// 2026-10-28 on the CI-pinned SQL Server, the shipped migrations applied,
	// same query returning 56 predicates for dbo), and an insert into and select
	// from admin.tenants succeed with no session context set. Reading it here
	// is for consistency with the counts, not because the table needs it.
	var tenantName string
	nameSQL, nameArgs, err := plugin.RebindArgs(`SELECT name FROM admin.tenants WHERE tenant_id = $1`, d.query, []any{tenantID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error rebinding tenant name lookup: %v\n", err)
		osExit(1)
	}
	if err := counter.QueryRowContext(ctx, nameSQL, nameArgs...).Scan(&tenantName); err != nil {
		if err == sql.ErrNoRows {
			tenantName = "(no admin.tenants row for this ID)"
		} else {
			fmt.Fprintf(os.Stderr, "error looking up tenant name: %v\n", err)
			osExit(1)
		}
	}

	counts, total, err := countTenantRows(ctx, counter, tables, tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error counting tenant data: %v\n", err)
		osExit(1)
	}

	fmt.Printf("Tenant: %s (%s)\n", tenantID, tenantName)
	fmt.Println("Rows that would be permanently deleted:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	printTenantRowCounts(w, tables, counts, total)

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

	// The two dialects take different arguments, not merely different
	// placeholder syntax, so this is a switch rather than a Rebind:
	// PostgreSQL's function takes the schema (cleat#1363) and SQL Server's
	// procedure has no schema to take.
	dropErr := func() error {
		switch d.name {
		case "mssql":
			// On the pinned connection, so that a failure leaves the same
			// session the counts were read on -- and because the procedure
			// restores whatever tenant key it found, which is the one set
			// above.
			dropSQL, dropArgs, err := plugin.RebindArgs(`EXEC admin.drop_tenant @tenant_id = $1`, d.query, []any{tenantID})
			if err != nil {
				return err
			}
			_, err = mssqlConn.ExecContext(ctx, dropSQL, dropArgs...)
			return err
		default:
			_, err := db.ExecContext(ctx, `SELECT admin.drop_tenant($1, $2)`, tenantID, schema)
			return err
		}
	}()
	if dropErr != nil {
		fmt.Fprintf(os.Stderr, "error dropping tenant: %v\n", dropErr)
		osExit(1)
	}

	// Audit record: this command has no dedicated audit table (see the
	// package doc comment above), so this printed summary -- the same
	// counts gathered before deletion, since every count is now zero -- is
	// the record of what was deleted.
	fmt.Printf("\nDeleted tenant %s (%s). Rows removed:\n", tenantID, tenantName)
	w2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	printTenantRowCounts(w2, tables, counts, total)
}

func printDropTenantUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl drop-tenant <tenant-id> [--dry-run] [--yes] [--schema=NAME]

Permanently delete a tenant and every row of its data: workflow_instances,
workflow_leases, workflow_payloads, event_history, workflow_signals,
workflow_promises, concurrency_keys, workflow_update_requests,
workflow_schedules, workflow_tags, workflow_routing, idempotency_keys,
tenant_settings, workflow_defs, admin.tenant_api_keys, admin.tenant_roles,
admin.tenants, plus the tenant's plugin schema and Postgres role.

--schema names the schema holding this deployment's cleat tables; it defaults
to "public" and must match cleat-worker's --schema. It is passed to
admin.drop_tenant explicitly rather than inferred, so the deletion cannot be
redirected by the connection's search_path (cleat#1363), and it also selects
the schema the row counts above are read from. It is PostgreSQL-only and is
refused on SQL Server, where every table this command touches is in dbo.

On SQL Server the table list above is read from sys.columns rather than
written down -- every table carrying a tenant_id column, which is the same set
migrations/mssql/074 deletes from, so the preview and the deletion cannot
disagree. Counting there requires the tenant's security-policy key, which this
command sets on one held connection; without it every count reads zero and a
tenant with data would preview as empty.

workflow_defs holds the tenant's uploaded WASM and IS tenant-owned -- its
primary key is (tenant_id, name, version). It used to be left behind; see
cleat#1201. Does not touch plugin_defs, which has no tenant_id column at all
(primary key (name, version)) and is a genuinely shared registry.

Refuses the default tenant
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
