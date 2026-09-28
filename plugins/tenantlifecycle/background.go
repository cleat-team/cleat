package tenantlifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

const sweepInterval = 60 * time.Second

// Run sweeps tenant_trials for tenants whose trial has expired and suspends
// them.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("tenant-lifecycle: no database, trial-expiry sweep disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	p.logger.Info("tenant-lifecycle: trial-expiry sweep started", "interval", sweepInterval)

	// Run once immediately on startup, matching scheduledbackup.Run,
	// plugins/scheduler's background loop and oauthprovider's -- a trial that
	// expired while the worker was down should not have to wait a full
	// interval after restart to be swept.
	p.sweep(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("tenant-lifecycle: trial-expiry sweep stopped")
			return nil
		case <-ticker.C:
			p.sweep(ctx)
		}
	}
}

// sweep suspends every tenant whose trial has expired and has not yet been
// handled.
//
// THE SCAN IS CROSS-TENANT, THE SAME SHAPE WEBHOOKINGEST'S RETRY SWEEP USES:
// "which trials anywhere have expired" has no tenant and cannot have one, so
// this needs plugin.AcrossAllTenants -- unlike Stage 1's original design,
// which read tenants through a host grant and so needed no bypass at all.
// tenant_trials is this plugin's own RLS-scoped table now, not admin.tenants,
// so the bypass is real and belongs in the ledger
// (a_cross_tenant_bypass_is_declared_test.go, kindGlobalSweep: the predicate
// is a cutoff -- expires_at < now() -- that belongs to no tenant).
//
// MARKED HERE, NOT IN Run -- DELIBERATELY, AND THIS WAS THE BUG THIS COMMENT
// REPLACES. The first version called plugin.AcrossAllTenants once at the top
// of Run and relied on the marked ctx flowing down through every p.sweep(ctx)
// call. That is correct for the only production caller, but TestSweep_Multi-
// Backend calls p.sweep(ctx) directly -- the natural way to unit-test the
// scan without also running Run's ticker loop -- with a bare, unmarked ctx.
// On PostgreSQL and MySQL this went unnoticed: the test's connection is the
// table owner, which PostgreSQL's RLS does not constrain by default, and
// MySQL has no row-level security at all, so an unmarked ctx and a correctly
// marked one read identically. SQL Server's security policy has no owner
// bypass -- measured directly, "sa" included -- so it was the one dialect
// where the omission was observable at all: sweep's own SELECT matched zero
// rows, silently, with no error, because dbo.fn_tenant_filter's FILTER
// PREDICATE hides every row from a session with neither tenant_id nor
// cross_tenant set in SESSION_CONTEXT. Root-caused by comparing a raw
// sp_set_session_context probe (rows visible) against the same query run
// through p.db under the test's own ctx (zero rows, no error) -- the gap
// between those two is exactly the marking this call now performs
// unconditionally, rather than trusting every caller to have done it first.
func (p *Plugin) sweep(ctx context.Context) {
	ctx = plugin.AcrossAllTenants(ctx,
		"tenant-lifecycle trial-expiry sweep: the scan for expired trials spans every tenant by definition")

	// now is computed ONCE, here, and bound as a parameter -- quota.go's
	// writeQuota does the same and for the same reason: MySQL's NOW()/NOW(6)
	// is evaluated in the SESSION's time zone, which need not be UTC, while
	// expires_at is written (settenanttrial.go) as a UTC value. Binding a Go
	// time.Time sidesteps the dialect's own "now" entirely, uniformly on all
	// three, rather than trusting SYSUTCDATETIME()/now()/NOW(6) to agree with
	// however the column was written.
	now := time.Now().UTC()
	rows, err := p.db.Query(ctx, queryExpiredTrials.For(p.dialect), now)
	if err != nil {
		p.logger.Error("tenant-lifecycle: query expired trials", "error", err)
		return
	}
	defer rows.Close()

	var expired []uuid.UUID
	for rows.Next() {
		var tenantID uuid.UUID
		var expiresAt time.Time
		if err := plugin.ScanRow(rows, &tenantID, &expiresAt); err != nil {
			p.logger.Error("tenant-lifecycle: scan trial row", "error", err)
			continue
		}
		expired = append(expired, tenantID)
	}
	if err := rows.Err(); err != nil {
		p.logger.Error("tenant-lifecycle: rows iteration error", "error", err)
		return
	}

	for _, tenantID := range expired {
		p.suspendExpiredTenant(ctx, tenantID)
	}
}

// suspendExpiredTenant suspends one tenant and marks its trial handled.
//
// ORDER MATTERS FOR CRASH SAFETY. Suspend first, mark handled second: if the
// process dies between the two, the next tick reads handled = false again
// and re-suspends -- idempotent, and a repeat suspend of an
// already-suspended tenant is a no-op write, not a symptom. The reverse
// order loses the suspension forever: marking handled first and crashing
// before the suspend leaves a tenant whose trial expired, was never
// suspended, and will never be looked at again, because the sweep's own
// predicate now excludes it.
//
// auth.ErrTenantNotFound IS CHECKED, AND MUST BE, OR THIS STARVES THE SWEEP.
// tenant_trials carries no foreign key to admin.tenants, so a row can
// outlive the tenant it names (the tenant was dropped after the trial was
// set). Treating that the same as a transient error -- log and leave
// unhandled for the next tick -- means the row is selected again every 60s
// forever, and queryExpiredTrials is `ORDER BY expires_at LIMIT 100`: a
// permanently-failing row keeps its early expires_at, so it keeps its slot
// in the 100 on every tick, and enough such rows silently starve every
// later trial out of the sweep entirely (cleat-review on #2590). There is
// nothing to retry here -- no tenant, ever, is going to appear -- so the row
// is marked handled (with a warning, since a dangling row is worth an
// operator's attention even though the sweep itself is done with it).
//
// Any OTHER error is left to retry on the next tick, as before: those are
// the transient case (a database blip) this design accepts the LIMIT-100
// starvation risk for, on the grounds that a real outage affecting more than
// 100 trials at once is itself the thing to be paged for, not something this
// loop should paper over by skipping rows within a tick.
func (p *Plugin) suspendExpiredTenant(ctx context.Context, tenantID uuid.UUID) {
	if p.env == nil || p.env.SetTenantSuspended == nil {
		// NIL MEANS "THIS HOST CANNOT DO IT" -- see plugin.Environment's doc
		// comment on SetTenantSuspended. Every real worker sets it; only
		// cleattest, the embedded runner and this plugin's own unit tests
		// leave it nil, and none of them run this loop unattended.
		p.logger.Error("tenant-lifecycle: SetTenantSuspended is not wired; cannot suspend expired trial",
			"tenant_id", tenantID)
		return
	}
	err := p.env.SetTenantSuspended(ctx, tenantID, true)
	switch {
	case err == nil:
		p.logger.Info("tenant-lifecycle: suspended tenant on trial expiry", "tenant_id", tenantID)
	case errors.Is(err, auth.ErrTenantNotFound):
		p.logger.Warn("tenant-lifecycle: trial row names a tenant that no longer exists; marking handled rather than retrying forever",
			"tenant_id", tenantID)
	default:
		p.logger.Error("tenant-lifecycle: suspend expired tenant", "tenant_id", tenantID, "error", err)
		return
	}
	if _, err := p.db.Exec(ctx, markTrialHandledSQL.For(p.dialect), tenantID); err != nil {
		// The tenant IS suspended (or confirmed gone) at this point; only the
		// bookkeeping failed. Logged rather than retried inline: the next
		// tick's scan still finds this row (handled is still false), so it
		// will try again -- the same idempotent-retry shape sweep() already
		// provides, and the same starvation risk this function's own doc
		// comment already accepts for a transient error.
		p.logger.Error("tenant-lifecycle: mark trial handled", "tenant_id", tenantID, "error", err)
	}
}

// queryExpiredTrials selects every unhandled, expired trial. $1/?/@p1 is a
// Go-computed `now`, bound by sweep -- see its comment for why this does not
// use any dialect's own now()/NOW(6)/SYSUTCDATETIME().
//
// handled = false/0 rather than NOT handled: T-SQL has no boolean type
// (CLAUDE.md's own recorded lesson, cleat#1133), so an explicit comparison is
// the one spelling that is unambiguous on all three.
var queryExpiredTrials = plugin.Query{
	Default: `SELECT tenant_id, expires_at FROM tenant_trials
WHERE expires_at < $1 AND handled = false
ORDER BY expires_at
LIMIT 100`,
	MySQL: `SELECT tenant_id, expires_at FROM tenant_trials
WHERE expires_at < $1 AND handled = false
ORDER BY expires_at
LIMIT 100`,
	MSSQL: `SELECT tenant_id, expires_at FROM tenant_trials
WHERE expires_at < $1 AND handled = 0
ORDER BY expires_at
OFFSET 0 ROWS FETCH NEXT 100 ROWS ONLY`,
}

// markTrialHandledSQL, like queryExpiredTrials, spells its boolean literal
// per dialect -- T-SQL accepts neither TRUE nor FALSE as a literal, only 0/1.
var markTrialHandledSQL = plugin.Query{
	Default: `UPDATE tenant_trials SET handled = true WHERE tenant_id = $1`,
	MySQL:   `UPDATE tenant_trials SET handled = true WHERE tenant_id = $1`,
	MSSQL:   `UPDATE tenant_trials SET handled = 1 WHERE tenant_id = $1`,
}
