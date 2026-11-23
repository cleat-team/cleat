package main

import (
	"context"
	"database/sql"
	"os"

	"github.com/cleat-team/cleat/engine"
)

// tenantCreationGuard answers whether creating one more tenant is refused on this worker's connection. A
// nil guard refuses nothing: every dialect but PostgreSQL, where the row-level security check does not
// run. It returns the refusal, or an error when it could not tell.
type tenantCreationGuard func(ctx context.Context) (*engine.RLSMultiTenantRefusal, error)

// multiTenantRefusalFor applies cleat#3772's rule to a connection whose RLS check has already run: it
// returns the refusal when reasons is non-empty (the connection is not subject to row-level security) and
// the database holds, or with creating would hold, more than one tenant.
//
// It counts only when there is something to refuse. A connection that is subject to the policies never
// reads admin.tenants here, so a worker on cleat_app pays nothing for it.
func multiTenantRefusalFor(ctx context.Context, db *sql.DB, reasons []engine.RLSBypassReason, creating bool) (*engine.RLSMultiTenantRefusal, error) {
	if len(reasons) == 0 {
		return nil, nil
	}
	role, err := engine.CurrentRole(ctx, db)
	if err != nil {
		return nil, err
	}
	n, err := engine.CountTenants(ctx, db)
	if err != nil {
		return nil, err
	}
	if creating {
		n++
	}
	return engine.DecideRLSMultiTenant(role, reasons, n, creating), nil
}

// tenantCreatedWarning is what `--create-tenant` prints after it has created a tenant: the warning when
// the connection bypasses row-level security and the database now holds more than one tenant, "" when it
// does not. It is called AFTER the creation, so the count already includes the new tenant. It never
// decides anything: the command's exit status is not its business (cleat#3772, owner decision D82 is about
// a connection that serves, and this one does not).
func tenantCreatedWarning(ctx context.Context, db *sql.DB) (string, error) {
	reasons, err := engine.CheckRLSEnforced(ctx, db)
	if err != nil {
		return "", err
	}
	refusal, err := multiTenantRefusalFor(ctx, db, reasons, false)
	if err != nil || refusal == nil {
		return "", err
	}
	return refusal.CreatedWarning(), nil
}

// newTenantCreationGuard builds the guard for a PostgreSQL connection. It interrogates the connection
// again on every call rather than trusting what startup saw, so it is right for a role changed under a
// running worker, and so that it applies under -rls-check=off, which skips the startup warning but not
// this.
func newTenantCreationGuard(db *sql.DB) tenantCreationGuard {
	return func(ctx context.Context) (*engine.RLSMultiTenantRefusal, error) {
		reasons, err := engine.CheckRLSEnforced(ctx, db)
		if err != nil {
			return nil, err
		}
		return multiTenantRefusalFor(ctx, db, reasons, true)
	}
}

// decideRuntimeMultiTenant answers the tick-time question: must a running worker stop, given the reasons
// its startup check recorded and the number of tenants this database HOLDS?
//
// It is a pure function of its arguments so the exit can be tested without os.Exit, and so the negative
// control -- a worker that is subject to row-level security, which serves any number of tenants
// legitimately -- is an argument rather than a mock.
//
// The count is engine.CountTenants: EVERY row of admin.tenants, SUSPENDED ones included, which is the
// same count the startup refusal uses. That was not the first design, and the first design was wrong. It
// reused the claim tick's own list, which excludes suspended tenants, on the reasoning that a suspended
// tenant is not served. Suspension stops STARTS, not reads (cmd/cleat-worker/server.go says so
// explicitly), and the plain listing and count are scoped by row-level security alone -- so on a
// bypassing connection one active tenant plus one suspended tenant is a database a restarted worker
// refuses to serve and a running one keeps serving indefinitely. The predicates have to match, and they
// are now byte-for-byte the same rule (cleat#3773 review).
func decideRuntimeMultiTenant(bypassReasons []engine.RLSBypassReason, tenantsHeld int) bool {
	return len(bypassReasons) > 0 && tenantsHeld > 1
}

// checkRuntimeTenantIsolation is the dispatch loop's guard: it stops the worker when the connection
// bypasses row-level security and the database holds more than one tenant. The caller must stop the loop
// when it returns true (in production exitFunc is os.Exit and does not return; the report is what makes
// the behaviour observable to a test).
//
// It runs at the TOP of every dispatch iteration, not inside a claim path, for two measured reasons
// (cleat#3773 review):
//
//   - the claim loop is only reached when there is free capacity, so a worker saturated with long runs
//     would never look; and
//   - `--claim-across-tenants=false` never enters claimRotating at all, and a worker in that mode still
//     READS across tenants on a bypassing connection.
//
// The cost is one count query per dispatch tick, paid ONLY when the startup check recorded a bypass
// reason. A worker subject to the policies returns on the first line and issues nothing.
//
// STOPPING, rather than refusing to claim, is the only action that makes the running state equal the state
// D82 already refuses at startup. A worker that stopped claiming but stayed up would keep serving its own
// tenant over routes whose plain reads are scoped by the policy alone -- the listing returns every
// tenant's rows on this connection -- which is weaker protection that looks equally deliberate.
// The cost is that one INSERT into admin.tenants becomes fleet-wide unavailability until an operator acts,
// and that is the cost D82 chose (cleat#3772, cleat#3773).
func (w *Worker) checkRuntimeTenantIsolation() bool {
	if len(w.rlsBypassReasons) == 0 {
		return false
	}
	if w.countTenantsForIsolation == nil {
		// Cannot happen in production: main() binds it whenever it records a
		// reason. A Worker built by a test without one measures nothing here,
		// and inventing a count would be worse than saying so.
		w.logger.WarnContext(w.ctx, "a worker on a connection that bypasses row-level security has no tenant counter, so the runtime multi-tenant guard cannot run",
			"worker_id", w.id)
		return false
	}
	tenants, err := w.countTenantsForIsolation(w.ctx)
	if err != nil {
		// FAILS OPEN when the tenants cannot be counted, as the startup check
		// does when it cannot count, and for the same reason: refusing on an
		// inconclusive read turns a database hiccup into a crash loop. The
		// count is retried on the next tick.
		w.logger.WarnContext(w.ctx, "could not count tenants, so the runtime refusal of a bypassing connection that serves more than one was not applied",
			"worker_id", w.id, "error", err)
		return false
	}
	if !decideRuntimeMultiTenant(w.rlsBypassReasons, tenants) {
		return false
	}
	role := w.rlsBypassRole
	if role == "" {
		role = "unknown"
	}
	// The same Message() the startup refusal prints, so an operator who has seen
	// one of these messages recognises the other.
	refusal := engine.DecideRLSMultiTenant(role, w.rlsBypassReasons, tenants, false)
	w.logger.ErrorContext(w.ctx, refusal.Message(), "worker_id", w.id, "tenants", tenants)
	if w.exitFunc != nil {
		w.exitFunc(1)
		return true
	}
	os.Exit(1)
	return true
}
