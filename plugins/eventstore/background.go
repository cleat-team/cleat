package eventstore

import (
	"context"
	"time"
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
			p.cleanup(ctx)
		}
	}
}

// cleanup runs a single round of event stream housekeeping.
func (p *Plugin) cleanup(ctx context.Context) {
	retentionDays := p.config.RetentionDays
	if retentionDays == 0 {
		retentionDays = 30 // default 30 days
	}
	if retentionDays < 0 {
		p.logger.Info("eventstore: cleanup skipped (retention disabled)")
		return
	}

	result, err := p.db.ExecContext(ctx,
		`DELETE FROM event_stream
		 WHERE created_at < NOW() - make_interval(days => $1)`,
		retentionDays,
	)
	if err != nil {
		p.logger.Error("eventstore: cleanup failed", "error", err)
		return
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		p.logger.Info("eventstore: cleanup completed",
			"deleted_events", n,
			"retention_days", retentionDays,
		)
	}
}
