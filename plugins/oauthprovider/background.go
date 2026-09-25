package oauthprovider

import (
	"context"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// sweepExpiredSessionsQuery removes oauth_sessions rows whose login was
// never completed: handleLogin (routes.go) inserts a row with state,
// code_verifier and nonce set and token_hash NULL, then handleCallback
// clears state/code_verifier/nonce and sets token_hash on success. A login
// that is abandoned -- the user never returns from the identity provider,
// or returns after the 5-minute PKCE window -- leaves a row with
// token_hash still NULL and expires_at in the past, forever, with nothing
// else in this plugin ever touching it again. cleat#2340's design review
// (cleat-review) named this an unbounded-growth MUST: the row count is
// driven entirely by how many logins are STARTED, not completed, which is
// not something an operator controls.
//
// token_hash IS NULL is the discriminator, not expires_at alone: a
// completed session's row also has expires_at in the past once its
// (separately-set, longer) session expiry elapses, and that row must not
// be swept here -- session expiry is enforced by extractSession checking
// expires_at at read time (middleware.go), not by deleting the row.
// Deleting a completed, merely-expired session here would be silently
// correct today (extractSession already refuses it) and would become
// silently wrong the moment anything needs a completed row after expiry
// (an audit trail, a "your last session was..." UI) -- token_hash IS NULL
// is what keeps this sweep scoped to exactly the rows the design named.
var sweepExpiredSessionsQuery = plugin.Query{
	Default: `
		DELETE FROM oauth_sessions
		WHERE token_hash IS NULL AND expires_at < now()`,
	MySQL: `
		DELETE FROM oauth_sessions
		WHERE token_hash IS NULL AND expires_at < NOW()`,
	MSSQL: `
		DELETE FROM oauth_sessions
		WHERE token_hash IS NULL AND expires_at < SYSUTCDATETIME()`,
}

// sweepInterval matches the PKCE state's own expiry window (handleLogin's
// sessionExpiresAt, routes.go): a row can only need sweeping once it has
// been abandoned for that long, so polling faster only adds load, and
// polling much slower lets abandoned rows sit for longer than the window
// itself.
const sweepInterval = 5 * time.Minute

// The worker reaches this loop only through a runtime type assertion --
// cmd/cleat-worker/main.go's `lp.Plugin.(plugin.HasBackground)` -- and a
// failed one is silent: no error, no log, and no test notices, because the
// sweep's own test calls sweepExpiredSessions directly rather than going
// through Run. So the abandoned-login rows would grow unbounded exactly as
// before, with a green suite.
//
// This pins the compile-time half: *Plugin must keep satisfying
// HasBackground, so removing Run or changing its signature is a build
// failure rather than a loop that quietly stops being started. (A value
// receiver on Run would not be such a change -- *Plugin's method set
// includes value-receiver methods either way -- and a constructor returning
// a Plugin instead of a *Plugin would not compile at all, since every method
// here including Info and Init is on the pointer.)
//
// What this line cannot see is which type the registry actually hands the
// worker; that is TestTheWorkerGetsAPluginThatCanRunTheSweep's half.
var _ plugin.HasBackground = (*Plugin)(nil)

// Run starts the oauth-provider background sweep loop, satisfying
// plugin.HasBackground. Every sweepInterval it deletes oauth_sessions rows
// left behind by an abandoned login (see sweepExpiredSessionsQuery). Returns
// promptly when ctx is cancelled.
//
// Runs on every dialect, not gated on p.dialect == plugin.DialectPostgres
// the way handleLogin/handleCallback are (see pgOnly in plugin.go): on
// mysql/mssql /login already refuses before any row is ever inserted, so
// the query here finds nothing and this is a correctly-behaving no-op --
// simpler than adding a second refusal path for a loop that is harmless to
// run everywhere, and consistent with scheduledbackup.Run, which also
// polls unconditionally and lets "nothing to do" fall out of an empty
// result rather than being special-cased at startup.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("oauth-provider: no database, background loop disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	p.logger.Info("oauth-provider: background loop started", "interval", sweepInterval)

	// Run once immediately on startup, matching scheduledbackup.Run and
	// plugins/scheduler's background loop -- an abandoned row left over
	// from before a restart should not have to wait a full interval to be
	// swept.
	p.sweepExpiredSessions(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("oauth-provider: background loop stopped")
			return nil

		case <-ticker.C:
			p.sweepExpiredSessions(ctx)
		}
	}
}

// sweepExpiredSessions deletes abandoned-login rows across every tenant.
// AcrossAllTenants, not ForTenant: this is a single global sweep over a
// table whose rows -- before completion -- exist precisely because the
// request that created them (handleLogin) has no authenticated tenant
// context of its own to scope by (see handleLogin's own ForTenant comment,
// routes.go), so there is no one tenant to hand this to either; it must
// see every tenant's abandoned rows in one pass, the same reasoning
// handleCallback's cross-tenant state lookup already applies.
func (p *Plugin) sweepExpiredSessions(ctx context.Context) {
	n, err := p.db.Exec(
		plugin.AcrossAllTenants(ctx, "oauth sweep: abandoned logins have no single tenant to scope by"),
		plugin.Rebind(sweepExpiredSessionsQuery.For(p.dialect), p.dialect))
	if err != nil {
		p.logger.Error("oauth-provider: sweep expired sessions", "error", err)
		return
	}
	if n > 0 {
		p.logger.Info("oauth-provider: swept abandoned login sessions", "count", n)
	}
}
