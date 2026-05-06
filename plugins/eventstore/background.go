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
	p.logger.Info("eventstore: cleanup")
}
