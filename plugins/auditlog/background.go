package auditlog

import (
	"context"
	"fmt"
	"time"
)

// Run starts the retention cleanup goroutine. It runs once per hour,
// deleting audit events older than the configured retention period.
// Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("audit-log: no database, retention cleanup disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	p.logger.Info("audit-log: retention cleanup started, interval=1h")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("audit-log: retention cleanup stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			affected, err := p.cleanupRetention(ctx)
			if err != nil {
				p.logger.Error("audit-log: retention cleanup failed",
					"plugin", p.Info().Name,
					"error", err,
				)
				continue
			}
			p.logger.Info("audit-log: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"deleted_events", affected,
			)
		}
	}
}

// cleanupRetention deletes audit events older than the configured retention period.
// Returns the number of deleted events.
func (p *Plugin) cleanupRetention(ctx context.Context) (int64, error) {
	retention := time.Duration(p.config.RetentionDays) * 24 * time.Hour
	cutoff := time.Now().Add(-retention)

	result, err := p.db.ExecContext(ctx, `
		DELETE FROM audit_events
		WHERE timestamp < $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old events: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		p.logger.Info("audit-log: deleted expired events",
			"count", affected,
			"cutoff", cutoff,
		)
	}
	return affected, nil
}
