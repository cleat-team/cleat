package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
)

// suspend-tenant / resume-tenant: the reversible half of tenant lifecycle.
//
// # Why this command exists
//
// `admin.tenants.suspended` has been in the schema since 001 and no Go code
// read it. A column named `suspended` that does nothing is worse than an
// absent one: the first operator to reach for it in an incident sets it, sees
// nothing happen, and has spent the minutes that mattered finding that out.
//
// # What suspension is, and what it is not
//
// It stops NEW work: no claim, no cron, and a 403 on start. It does not touch
// runs that are already executing -- those finish, heartbeating normally, and
// reach a terminal state. And it does not affect reads at all: a tenant
// suspended for non-payment can still see its own runs and history, because
// blocking that punishes the wrong thing.
//
// To stop work already running, cancel it. Suspension and cancellation are
// different instruments; conflating them would make the reversible one
// destructive.
//
// drop-tenant is the irreversible end of the same lifecycle, and this is what
// was missing between "nothing" and "gone": a non-paying customer, a tenant
// being investigated for abuse, or an offboarding grace period all want the
// data kept and the work stopped.
const suspendTenantUsage = `usage: cleatctl suspend-tenant <tenant-id> [--yes]
       cleatctl resume-tenant <tenant-id>

Suspending stops NEW work for a tenant:
  - its workflows are no longer claimed
  - its cron schedules no longer fire
  - starting a workflow returns 403

It does NOT stop runs already executing -- they finish normally -- and it does
not affect reads. Use cancel for work in flight, and drop-tenant to delete.

Flags:
  --yes       skip the confirmation prompt (suspend only)
`

func runSuspendTenant(ctx context.Context, db *sql.DB, d dialect, args []string, suspend bool) {
	name := "resume-tenant"
	if suspend {
		name = "suspend-tenant"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", suspendTenantUsage) }
	yes := fs.Bool("yes", false, "skip the confirmation prompt")

	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "error: exactly one tenant id is required\n\n%s", suspendTenantUsage)
		osExit(2)
		return
	}
	tenantID := fs.Arg(0)

	// The row is read first so the command can say what it is about to change
	// rather than reporting a rowcount. A tenant that is ALREADY in the
	// requested state is reported and not written: rewriting it would look
	// like an action was taken, and on a shared operational timeline "who
	// suspended this and when" is the question being asked.
	var current bool
	var tenantName string
	err := db.QueryRowContext(ctx, d.rebind(
		`SELECT name, suspended FROM admin.tenants WHERE tenant_id = $1`),
		tenantID).Scan(&tenantName, &current)
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

	if current == suspend {
		state := "not suspended"
		if current {
			state = "already suspended"
		}
		fmt.Printf("tenant %s (%s) is %s; nothing to do\n", tenantID, tenantName, state)
		return
	}

	if suspend && !*yes {
		fmt.Printf("Suspend tenant %s (%s)? Its workflows stop being claimed and its cron stops firing.\n",
			tenantID, tenantName)
		fmt.Printf("Runs already executing will finish. Type the tenant name to confirm: ")
		var typed string
		_, _ = fmt.Scanln(&typed)
		if typed != tenantName {
			fmt.Fprintln(os.Stderr, "not confirmed; nothing changed")
			osExit(1)
			return
		}
	}

	if _, err := db.ExecContext(ctx, d.rebind(
		`UPDATE admin.tenants SET suspended = $1 WHERE tenant_id = $2`),
		suspend, tenantID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	if suspend {
		fmt.Printf("tenant %s (%s) suspended. Workers stop claiming its work within one dispatch tick.\n",
			tenantID, tenantName)
		fmt.Printf("Runs already executing finish normally; cancel them if that is not what you want.\n")
		return
	}
	fmt.Printf("tenant %s (%s) resumed. Its queued work is claimed again within one dispatch tick.\n",
		tenantID, tenantName)
}
