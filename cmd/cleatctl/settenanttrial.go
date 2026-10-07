package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
)

// set-tenant-trial: the operator-only write path for
// plugins/tenantlifecycle's trial-expiry sweep. cleat#2534.
//
// # Why this is operator-only, and why that is load-bearing
//
// A workflow's own host-call surface could, in principle, set its own
// tenant's trial expiry -- and the first design of this feature did exactly
// that, before review caught the bug: a tenant's OWN guest code (which a
// tenant can supply itself, see docs/playbooks/b2b-saas-control-plane.md's
// "Tenant-supplied steps") could then indefinitely postpone its own trial by
// re-recording a later expiry, defeating the sweep it was meant to feed. The
// enforcement mechanism cannot be controlled by the party it constrains.
// So: cleatctl only. There is no host function that reaches tenant_trials.
const setTenantTrialUsage = `usage: cleatctl set-tenant-trial <tenant-id> --days N

Sets the tenant's trial to expire N days from now. plugins/tenantlifecycle's
background sweep suspends the tenant once its trial has expired
(cleatctl suspend-tenant does the same thing by hand, immediately).

Extending an already-expired-and-suspended tenant's trial resets it back into
the unhandled state, so the sweep leaves it alone until the new expiry --
but does NOT resume the tenant itself. Resuming and extending are different
instruments, matching suspend-tenant/resume-tenant's own separation: run
  cleatctl resume-tenant <tenant-id>
if you also want the tenant working again immediately.

Flags:
  --days N    trial length in days from now (required, must be positive)
`

func runSetTenantTrial(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("set-tenant-trial", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", setTenantTrialUsage) }
	days := fs.Int("days", 0, "trial length in days from now")

	// parseFlagsAnywhere, not fs.Parse -- suspend-tenant's own comment
	// explains why (cleat#1933): the flag package stops at the first
	// operand, and `set-tenant-trial <uuid> --days 14` has the operand
	// first.
	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(2)
		return
	}
	if len(operands) != 1 {
		fmt.Fprintf(os.Stderr, "error: exactly one tenant id is required\n\n%s", setTenantTrialUsage)
		osExit(2)
		return
	}
	if *days <= 0 {
		fmt.Fprintf(os.Stderr, "error: --days must be positive, got %d\n\n%s", *days, setTenantTrialUsage)
		osExit(2)
		return
	}
	tenantID, err := uuid.Parse(operands[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: not a tenant UUID: %q: %v\n", operands[0], err)
		osExit(1)
		return
	}

	// Read the tenant's name first, from admin.tenants -- not because this
	// command touches that table, but so a typo'd tenant id is refused with
	// "no such tenant" rather than silently writing an orphan trial row for
	// nobody. Mirrors suspend-tenant's own read-before-write.
	tenantName, err := lookupTenantName(ctx, db, d, tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	expiresAt := time.Now().UTC().AddDate(0, 0, *days)

	// Writes directly into plugins/tenantlifecycle's own tenant_trials table
	// -- the one deliberate core-into-plugin-schema wrinkle in this design.
	// There is precedent: `cleatctl quota` already writes tenantquota's
	// TenantScoped tenant_quota table the same way (quota.go). Splitting
	// trial expiry across a core column and a plugin's sweep was considered
	// and rejected: a column enforced only when an optional plugin happens
	// to be running does nothing while it is off, which is the exact defect
	// admin.tenants.suspended sat as for years before suspend-tenant existed
	// (see that command's own doc comment). Keeping both in the plugin means
	// the column always means something.
	exec, closeExec, err := tenantTrialConnFor(ctx, db, d, tenantID.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	defer closeExec()

	if err := writeTenantTrial(ctx, exec, d, tenantID, expiresAt); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	fmt.Printf("tenant %s (%s) trial set to expire %s (in %d days). "+
		"It will be suspended by the trial-expiry sweep after that, or immediately with suspend-tenant.\n",
		tenantID, tenantName, expiresAt.Format(time.RFC3339), *days)
}

// writeTenantTrial does the actual read-then-write, split out from
// runSetTenantTrial the way oauthAllowList is split from its own printing
// loop (oauthallow.go): the part worth testing against a real database is
// the one that returns an error, and a function that prints and calls
// osExit cannot be called from a test at all.
//
// Read-then-write, exactly quota.go's writeQuota shape -- NOT a
// dialect-specific upsert (ON CONFLICT / ON DUPLICATE KEY / MERGE all spell
// "insert or update" differently, and TestEveryInlineStatementParsesOnPostgres
// caught the first version of this function trying to parse MySQL's spelling
// against Postgres). A plain INSERT/UPDATE pair, bound through d.rebindArgs,
// is portable across all three -- `false` is a bound Go value here, not
// literal SQL text, so plugin.Rebind's own true/false -> 1/0 rewrite for
// MSSQL (plugin/query.go) applies to it exactly as it would to any other
// argument.
func writeTenantTrial(ctx context.Context, exec quotaExecer, d dialect, tenantID uuid.UUID, expiresAt time.Time) error {
	hasRow, err := tenantTrialExists(ctx, exec, d, tenantID)
	if err != nil {
		return err
	}
	if hasRow {
		stmt, stmtArgs, err := d.rebindArgs(
			`UPDATE tenant_trials SET expires_at = $1, handled = $2 WHERE tenant_id = $3`,
			expiresAt, false, tenantID)
		if err != nil {
			return err
		}
		if _, err := exec.ExecContext(ctx, stmt, stmtArgs...); err != nil {
			return err
		}
		return nil
	}

	stmt, stmtArgs, err := d.rebindArgs(
		`INSERT INTO tenant_trials (tenant_id, expires_at, handled) VALUES ($1, $2, $3)`,
		tenantID, expiresAt, false)
	if err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, stmt, stmtArgs...); err != nil {
		// isQuotaDuplicateKey (quota.go): the same TOCTOU window writeQuota's
		// INSERT branch has, between the existence check above and this
		// INSERT -- a second `set-tenant-trial` for the same tenant racing
		// this one. Rare for an operator command, but the friendly message
		// is cheap and the raw constraint-violation text is not.
		if isQuotaDuplicateKey(err) {
			return fmt.Errorf("tenant %s's trial row was created by another command "+
				"while this one was running; re-run set-tenant-trial to update it", tenantID)
		}
		return err
	}
	return nil
}

// lookupTenantName reads a tenant's display name, refusing with a clear
// error if the id matches no row -- the same read suspend-tenant does before
// its own write.
func lookupTenantName(ctx context.Context, db *sql.DB, d dialect, tenantID uuid.UUID) (string, error) {
	stmt, stmtArgs, err := d.rebindArgs(`SELECT name FROM admin.tenants WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return "", err
	}
	var name string
	err = db.QueryRowContext(ctx, stmt, stmtArgs...).Scan(&name)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no tenant %s in admin.tenants", tenantID)
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// tenantTrialConnFor is quotaConnFor's twin for tenant_trials, per
// cleat-review on cleat#2534: SQL Server's security policy on a TenantScoped
// table refuses a write with no SESSION_CONTEXT('tenant_id') set on the
// connection making it, and that key is per-connection state go-mssqldb
// clears on ResetSession, so a write through the bare pool would pass
// silently on PostgreSQL and MySQL and be refused on MSSQL alone. See
// quota.go's quotaConnFor for the full reasoning; a shared helper is a
// reasonable follow-up refactor once a third caller wants this.
func tenantTrialConnFor(ctx context.Context, db *sql.DB, d dialect, tenantID string) (quotaExecer, func(), error) {
	if d.name != "mssql" {
		return db, func() {}, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquiring a connection: %w", err)
	}
	if err := setMSSQLTenantKey(ctx, conn, tenantID); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, func() { conn.Close() }, nil
}

// tenantTrialExists reports whether tenantID already has a tenant_trials
// row, so runSetTenantTrial can choose INSERT or UPDATE the way
// quota.go's writeQuota does -- read-modify-write rather than a
// dialect-specific upsert.
func tenantTrialExists(ctx context.Context, exec quotaExecer, d dialect, tenantID uuid.UUID) (bool, error) {
	stmt, stmtArgs, err := d.rebindArgs(`SELECT 1 FROM tenant_trials WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return false, err
	}
	var one int
	err = exec.QueryRowContext(ctx, stmt, stmtArgs...).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
