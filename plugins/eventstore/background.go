package eventstore

import (
	"context"
	"time"

	"github.com/cleat-team/cleat/internal/plugin"
)

// Run starts the periodic cleanup goroutine. It logs a cleanup message on a
// 1-hour ticker and respects context cancellation. Returns when ctx is done.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("eventstore: no database, cleanup disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	p.logger.Info("eventstore: cleanup started, interval=1h")

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
