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

// dropTenantTableCounts mirrors the rows dropping a tenant removes, for the
// dry-run / confirmation / audit output. Keep this list in sync with
// migrations/postgres/032_drop_tenant_deletes_tenant_data.sql.
//
// "Removes", not "admin.drop_tenant deletes", because two of these go by
// foreign key rather than by a DELETE inside the function, and the operator
// does not care which mechanism took their data:
//
//   - tenant_settings, ON DELETE CASCADE from 039_tenant_settings.sql
//   - tenant_domains, ON DELETE CASCADE from
//     080_a_hostname_belongs_to_one_tenant.sql (cleat#1568)
//   - tenant_secrets, ON DELETE CASCADE from
//     081_a_secret_never_reaches_the_guest.sql (cleat#1570)
//   - workflow_defs, ON DELETE CASCADE from
//     059_a_dropped_tenants_definitions_go_with_it.sql (cleat#1201)
//   - queues, ON DELETE CASCADE from
//     093_a_queue_declares_its_own_concurrency_limit.sql (cleat#1116)
//
// All cascade off admin.tenants, which drop_tenant deletes LAST -- that
// ordering is what 039 relied on and what 059 relies on. Reading only the
// function's DELETE statements to maintain this list is how tenant_settings
// came to be missing from it for twenty migrations.
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
	{"tenant_settings", `SELECT count(*) FROM tenant_settings WHERE tenant_id = $1`},
	// Also ON DELETE CASCADE rather than a DELETE inside admin.drop_tenant --
	// from 080_a_hostname_belongs_to_one_tenant.sql, which followed 039's
	// reasoning deliberately. Listed here for the same reason 039 is: the
	// operator wants the count, not the mechanism.
	{"tenant_domains", `SELECT count(*) FROM tenant_domains WHERE tenant_id = $1`},
	// ON DELETE CASCADE from 081_a_secret_never_reaches_the_guest.sql. Listed
	// for the count, as tenant_settings is -- and this one matters more than
	// most: an operator deleting a tenant wants to see that its credentials
	// went with it.
	{"tenant_secrets", `SELECT count(*) FROM tenant_secrets WHERE tenant_id = $1`},
	// ON DELETE CASCADE from migrations/postgres/093 (cleat#1116). Listed
	// for the count, as tenant_domains and tenant_secrets are above: a
	// dropped tenant's declared concurrency limits go with it, and the
	// operator wants to see that.
	{"queues", `SELECT count(*) FROM queues WHERE tenant_id = $1`},
	// ON DELETE CASCADE from workflow_instances, via migrations/postgres/094
	// (cleat#1116's semaphore holder). Listed for the count, as queues is
	// above: a dropped tenant's queue holders go with its workflow_instances,
	// and the operator wants to see that.
	{"queue_holders", `SELECT count(*) FROM queue_holders WHERE tenant_id = $1`},
	// ON DELETE CASCADE from admin.tenants directly, via
	// migrations/postgres/097 (cleat#1918's rate-limit counter). Deliberately
	// NOT chained off workflow_instances the way queue_holders is -- a rate
	// token's lifetime must not depend on how long completed workflows are
	// retained, so it carries its own tenant FK. Listed for the count, as
	// queue_holders is above.
	{"queue_rate_tokens", `SELECT count(*) FROM queue_rate_tokens WHERE tenant_id = $1`},
	// cleat#1644. Both carry tenant_id since 056 and neither has a foreign key
	// to anything, so neither was deleted OR counted: a dropped tenant's memory
	// profile -- which workflows it ran, and how much memory each used --
	// survived, and did not appear in the preview an operator confirms.
	{"workflow_memory_stats", `SELECT count(*) FROM workflow_memory_stats WHERE tenant_id = $1`},
	{"workflow_memory_samples", `SELECT count(*) FROM workflow_memory_samples WHERE tenant_id = $1`},
	{"workflow_defs", `SELECT count(*) FROM workflow_defs WHERE tenant_id = $1`},
	{"admin.tenant_api_keys", `SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = $1`},
	{"admin.tenant_roles", `SELECT count(*) FROM admin.tenant_roles WHERE tenant_id = $1`},
	// ON DELETE CASCADE from admin.tenants, like tenant_settings above and for
	// the same reason: the operator wants the count, not the mechanism. Found
	// by the coverage test rather than by reading the function -- it is deleted
	// correctly and was simply never counted. cleat#1644.
	{"admin.tenant_egress_allow", `SELECT count(*) FROM admin.tenant_egress_allow WHERE tenant_id = $1`},
	// ON DELETE CASCADE from migrations/postgres/104 (cleat#2230). Listed for
	// the count, as tenant_domains and tenant_secrets are above: a dropped
	// tenant's Slack workspace mapping(s) go with it, and the operator wants
	// to see that.
	{"slack_workspace", `SELECT count(*) FROM slack_workspace WHERE tenant_id = $1`},
	{"admin.tenants", `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`},
}

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
	// (migrations/postgres/032_drop_tenant_deletes_tenant_data.sql) --
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

	// Through counter, not db: on SQL Server admin.tenants carries the same
	// policy as everything else, so the name is only readable on the connection
	// whose tenant key was set above.
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
event_history, workflow_signals, workflow_promises, concurrency_keys,
workflow_update_requests, workflow_schedules, workflow_tags,
workflow_routing, idempotency_keys, tenant_settings, workflow_defs,
admin.tenant_api_keys, admin.tenant_roles, admin.tenants, plus the tenant's
plugin schema and Postgres role.

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
