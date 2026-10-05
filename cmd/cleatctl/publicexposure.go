package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/auth"
	"github.com/google/uuid"
)

// allow-public-exposure / revoke-public-exposure: the operator opt-in
// cleat#1986's design asks for -- "public needs a per-tenant operator opt-in
// and is refused at deploy otherwise". Mirrors suspend-tenant/resume-tenant's
// shape exactly: a read of the current state first, a no-op reported rather
// than rewritten if nothing changes, and the write goes through
// auth.TenantStore so this file carries no SQL of its own (cleat#866 is what
// two implementations of one write path disagreeing about where a row lives
// looks like).
//
// GRANTING gets a confirmation prompt; REVOKING does not -- the same
// asymmetry as suspend (destructive-looking) versus resume (not), because
// granting this is the one direction that weakens anything: it lets the
// tenant deploy a workflow reachable with no credential at all. Revoking it
// only ever narrows what a future deploy may declare; it does not reach back
// and un-deploy an already-public definition.
const publicExposureUsage = `usage: cleatctl allow-public-exposure <tenant-id> [--yes]
       cleatctl revoke-public-exposure <tenant-id>

Grants or revokes a tenant's operator opt-in for the 'public' exposure class
(cleat#1986). Without this grant, a deploy whose resolved exposure class is
'public' is refused rather than silently stored as 'auth'.

This does not retroactively affect an already-deployed version -- it bounds
what a FUTURE deploy may store.

Flags:
  --yes       skip the confirmation prompt (allow only)
`

func runPublicExposureGrant(ctx context.Context, db *sql.DB, d dialect, args []string, allow bool) {
	name := "revoke-public-exposure"
	if allow {
		name = "allow-public-exposure"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", publicExposureUsage) }
	yes := fs.Bool("yes", false, "skip the confirmation prompt")

	// parseFlagsAnywhere, not fs.Parse -- see suspendtenant.go's identical
	// comment and cleat#1933, the bug it exists to avoid.
	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(2)
		return
	}
	if len(operands) != 1 {
		fmt.Fprintf(os.Stderr, "error: exactly one tenant id is required\n\n%s", publicExposureUsage)
		osExit(2)
		return
	}
	tenantID := operands[0]

	var current bool
	var tenantName string
	stmt, stmtArgs, err := d.rebindArgs(
		`SELECT name, allow_public_exposure FROM admin.tenants WHERE tenant_id = $1`,
		tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	err = db.QueryRowContext(ctx, stmt, stmtArgs...).Scan(&tenantName, &current)
	if err == sql.ErrNoRows {
		fmt.Fprintf(os.Stderr, "error: no tenant %s in admin.tenants\n", tenantID)
		osExit(1)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	if current == allow {
		state := "not opted in"
		if current {
			state = "already opted in"
		}
		fmt.Printf("tenant %s (%s) is %s to the public exposure class; nothing to do\n", tenantID, tenantName, state)
		return
	}

	if allow && !*yes {
		fmt.Printf("Allow tenant %s (%s) to deploy workflows declared 'public' -- reachable with NO credential?\n",
			tenantID, tenantName)
		fmt.Printf("This does not expose anything by itself; it only lifts the deploy-time refusal. Type the tenant name to confirm: ")
		var typed string
		_, _ = fmt.Scanln(&typed)
		if typed != tenantName {
			fmt.Fprintln(os.Stderr, "not confirmed; nothing changed")
			osExit(1)
			return
		}
	}

	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: not a tenant UUID: %q: %v\n", tenantID, err)
		osExit(1)
		return
	}
	store, err := auth.NewTenantStoreForDialect(db, d.name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if err := store.SetTenantAllowsPublicExposure(ctx, tenantUUID, allow); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	if allow {
		fmt.Printf("tenant %s (%s) may now deploy workflows declared 'public'.\n", tenantID, tenantName)
		return
	}
	fmt.Printf("tenant %s (%s) may no longer deploy workflows declared 'public'. Already-deployed versions are unaffected.\n",
		tenantID, tenantName)
}
