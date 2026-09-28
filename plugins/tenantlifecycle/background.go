package tenantlifecycle

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

const sweepInterval = 60 * time.Second

// Run sweeps tenant_trials for tenants whose trial has expired and suspends
// them.
//
// THE SCAN IS CROSS-TENANT, THE SAME SHAPE WEBHOOKINGEST'S RETRY SWEEP USES:
// "which trials anywhere have expired" has no tenant and cannot have one, so
// this needs plugin.AcrossAllTenants -- unlike Stage 1's original design,
// which read tenants through a host grant and so needed no bypass at all.
// tenant_trials is this plugin's own RLS-scoped table now, not admin.tenants,
// so the bypass is real and belongs in the ledger
// (a_cross_tenant_bypass_is_declared_test.go, kindGlobalSweep: the predicate
// is a cutoff -- expires_at < now() -- that belongs to no tenant).
func (p *Plugin) Run(ctx context.Context) error {
	ctx = plugin.AcrossAllTenants(ctx,
		"tenant-lifecycle trial-expiry sweep: the scan for expired trials spans every tenant by definition")

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
func (p *Plugin) sweep(ctx context.Context) {
	rows, err := p.db.Query(ctx, queryExpiredTrials.For(p.dialect))
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
// and re-suspends -- idempotent, and SetTenantSuspended's own
// ErrTenantNotFound aside, a repeat suspend of an already-suspended tenant is
// a no-op write, not a symptom. The reverse order loses the suspension
// forever: marking handled first and crashing before the suspend leaves a
// tenant whose trial expired, was never suspended, and will never be looked
// at again, because the sweep's own predicate now excludes it.
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
	if err := p.env.SetTenantSuspended(ctx, tenantID, true); err != nil {
		p.logger.Error("tenant-lifecycle: suspend expired tenant", "tenant_id", tenantID, "error", err)
		return
	}
	if _, err := p.db.Exec(ctx, markTrialHandledSQL.For(p.dialect), tenantID); err != nil {
		// The tenant IS suspended at this point; only the bookkeeping failed.
		// Logged rather than retried inline: the next tick's scan still
		// finds this row (handled is still false), so it will try again --
		// the same idempotent-retry shape sweep() already provides.
		p.logger.Error("tenant-lifecycle: mark trial handled", "tenant_id", tenantID, "error", err)
		return
	}
	p.logger.Info("tenant-lifecycle: suspended tenant on trial expiry", "tenant_id", tenantID)
}

// queryExpiredTrials selects every unhandled, expired trial.
//
// handled = false/0 rather than NOT handled: T-SQL has no boolean type
// (CLAUDE.md's own recorded lesson, cleat#1133), so an explicit comparison is
// the one spelling that is unambiguous on all three.
var queryExpiredTrials = plugin.Query{
	Default: `SELECT tenant_id, expires_at FROM tenant_trials
WHERE expires_at < now() AND handled = false
ORDER BY expires_at
LIMIT 100`,
	MySQL: `SELECT tenant_id, expires_at FROM tenant_trials
WHERE expires_at < NOW(6) AND handled = false
ORDER BY expires_at
LIMIT 100`,
	MSSQL: `SELECT tenant_id, expires_at FROM tenant_trials
WHERE expires_at < SYSUTCDATETIME() AND handled = 0
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
