package auditlog

import (
	"context"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// Run starts the background goroutine. It periodically drains the audit
// event buffer and runs retention cleanup. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("audit-log: no database, retention cleanup disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	drainTicker := time.NewTicker(1 * time.Second)
	defer drainTicker.Stop()

	defer func() {
		p.drainBuffer()
	}()

	p.logger.Info("audit-log: background started, retention=1h, drain=1s")

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

		case <-drainTicker.C:
			p.drainBuffer()
		}
	}
}

// drainBuffer drains queued audit events from the buffer and records them.
// It processes up to 100 events per call and returns early when the
// buffer is empty.
func (p *Plugin) drainBuffer() {
	if p.buffer == nil {
		return
	}
	for i := 0; i < 100; i++ {
		select {
		case evt := <-p.buffer:
			p.recordAudit(context.Background(),
				evt.tenantID, evt.method, evt.path,
				evt.statusCode, evt.ipAddress, evt.userAgent,
				evt.duration,
			)
		default:
			return
		}
	}
}

// cleanupRetention deletes audit events older than the configured retention period.
// Returns the number of deleted events.
func (p *Plugin) cleanupRetention(ctx context.Context) (int64, error) {
	retention := time.Duration(p.config.RetentionDays) * 24 * time.Hour
	cutoff := time.Now().Add(-retention)

	result, err := p.db.Exec(ctx, plugin.Rebind(`
			DELETE FROM audit_events
			WHERE timestamp < $1
		`, p.dialect), cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old events: %w", err)
	}
	if result > 0 {
		p.logger.Info("audit-log: deleted expired events",
			"count", result,
			"cutoff", cutoff,
		)
	}
	return result, nil
}
