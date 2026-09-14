package eventstore

import (
	"context"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// cleanupInterval is how often Run sweeps expired events.
//
// A var rather than a literal so that the test which proves Run marks its own
// sweep can drive a real tick. At an hour, nothing in a test can make the loop
// fire, so the AcrossAllTenants call below would be covered by no test at all
// -- and it is the one line whose absence breaks the worker outright rather
// than degrading it. cleat#1512.
var cleanupInterval = time.Hour

// Run starts the periodic cleanup goroutine. It sweeps on cleanupInterval and
// respects context cancellation. Returns when ctx is done.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("eventstore: no database, cleanup disabled")
		<-ctx.Done()
		return nil
	}

	// THE SWEEP NAMES ITSELF CROSS-TENANT. cleat#1512. event_stream carries a
	// row-level policy from migration v2, and the policy calls
	// cleat.assert_tenant_set(), which RAISEs rather than filtering when no
	// tenant is in scope. Without this the loop does not degrade -- it fails
	// outright on its first statement after the migration lands.
	//
	// AcrossAllTenants rather than ForTenant, because there is no tenant to be
	// had: cleanup deletes by AGE across every stream, and a retention policy
	// that ran per-tenant would need a tenant it was never given. The
	// discrimination matters -- a writer that HAS a tenant and drops it on the
	// way to context.Background() wants ForTenant, and bypassing there compiles,
	// passes every test, and silently disables isolation on that path.
	//
	// Marked ONCE here rather than inside cleanup(), because every statement
	// reachable from this function is cross-tenant for the same reason. The
	// three handlers in routes.go are deliberately NOT marked: they run on
	// r.Context(), which carries the request's tenant. Nor is the poll loop
	// INSIDE handleSSE -- a 1s ticker in a for/select, which reads exactly like
	// a background loop and is not one. Its context is still the request's, so
	// marking it would widen every SSE read to every tenant.
	ctx = plugin.AcrossAllTenants(ctx,
		"eventstore cleanup: retention deletes events by age across every tenant's streams")

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	p.logger.Info("eventstore: cleanup started", "interval", cleanupInterval)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("eventstore: cleanup stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			n := p.cleanup(ctx)
			p.logger.Info("eventstore: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"deleted_events", n,
			)
		}
	}
}

// cleanup runs a single round of event stream housekeeping and returns the
// number of deleted events.
func (p *Plugin) cleanup(ctx context.Context) int64 {
	retentionDays := p.config.RetentionDays
	if retentionDays == 0 {
		retentionDays = 30 // default 30 days
	}
	if retentionDays < 0 {
		p.logger.Info("eventstore: cleanup skipped (retention disabled)")
		return 0
	}

	result, err := p.db.Exec(ctx, plugin.Rebind(deleteEventsOlderThan.For(p.dialect), p.dialect),
		retentionDays,
	)
	if err != nil {
		p.logger.Error("eventstore: cleanup failed", "error", err)
		return 0
	}
	if result > 0 {
		p.logger.Info("eventstore: cleanup completed",
			"deleted_events", result,
			"retention_days", retentionDays,
		)
	}
	return result
}
