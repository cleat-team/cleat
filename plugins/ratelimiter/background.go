package ratelimiter

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Run starts the background rate limit config reload loop. It queries all
// rate limit configurations from the database every 30 seconds and rebuilds
// the in-memory token bucket cache. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("rate-limiter: no database, background reload disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	p.logger.Info("rate-limiter: background reload started, interval=30s")

	// Do an initial load so the middleware has data immediately.
	if err := p.reload(ctx); err != nil {
		p.logger.Error("rate-limiter: initial reload failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("rate-limiter: background reload stopped")
			return nil

		case <-ticker.C:
			if err := p.reload(ctx); err != nil {
				p.logger.Error("rate-limiter: reload failed", "error", err)
			}
		}
	}
}

// reload queries all rate limit configs from the database and rebuilds the
// in-memory token bucket map atomically under the plugin mutex.
func (p *Plugin) reload(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT tenant_id, limit_key, max_requests, window_seconds
		FROM rate_limits
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	newBuckets := make(map[string]*tokenBucket)
	for rows.Next() {
		var tid uuid.UUID
		var limitKey string
		var maxRequests, windowSeconds int
		if err := rows.Scan(&tid, &limitKey, &maxRequests, &windowSeconds); err != nil {
			p.logger.Error("rate-limiter: scan row", "error", err)
			continue
		}
		key := tid.String() + "/" + limitKey
		newBuckets[key] = newTokenBucket(maxRequests, windowSeconds)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	p.buckets = newBuckets
	p.mu.Unlock()

	p.logger.Debug("rate-limiter: reloaded rate limits", "count", len(newBuckets))
	return nil
}
