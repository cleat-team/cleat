package ratelimiter

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/rcownie/cleat/internal/plugin"
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

	pruneTicker := time.NewTicker(5 * time.Minute)
	defer pruneTicker.Stop()

	p.logger.Info("rate-limiter: background reload started, interval=30s")

	// Do an initial load so the middleware has data immediately.
	start := time.Now()
	n, err := p.reload(ctx)
	if err != nil {
		p.logger.Error("rate-limiter: initial reload failed", "error", err)
	} else {
		p.logger.Info("rate-limiter: work cycle completed",
			"plugin", p.Info().Name,
			"duration_ms", time.Since(start).Milliseconds(),
			"configs_reloaded", n,
		)
	}

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("rate-limiter: background reload stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			n, err := p.reload(ctx)
			if err != nil {
				p.logger.Error("rate-limiter: reload failed",
					"plugin", p.Info().Name,
					"error", err,
				)
				continue
			}
			p.logger.Info("rate-limiter: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"configs_reloaded", n,
			)

		case <-pruneTicker.C:
			p.pruneRateCounters(ctx)
		}
	}
}

// reload queries all rate limit configs from the database and rebuilds the
// in-memory token bucket map atomically under the plugin mutex.
// Returns the number of configs reloaded.
func (p *Plugin) reload(ctx context.Context) (int, error) {
	rows, err := p.db.Query(ctx, plugin.Rebind(`
		SELECT tenant_id, limit_key, max_requests, window_seconds
		FROM rate_limits
	`, p.dialect))
	if err != nil {
		return 0, err
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
		return 0, err
	}

	p.mu.Lock()
	p.buckets = newBuckets
	p.mu.Unlock()

	count := len(newBuckets)
	p.logger.Debug("rate-limiter: reloaded rate limits", "count", count)
	return count, nil
}

// pruneRateCounters deletes rate_counter rows with window_start older than
// 2× the maximum configured window_seconds (or 5 minutes, whichever is larger).
// This prevents unbounded growth of the rate_counter table without accidentally
// pruning active windows.
func (p *Plugin) pruneRateCounters(ctx context.Context) {
	maxWindow := 60 // default if no configs loaded
	p.mu.Lock()
	for _, b := range p.buckets {
		w := int(b.maxTokens / b.refillRate)
		if w > maxWindow {
			maxWindow = w
		}
	}
	p.mu.Unlock()

	cutoff := time.Now().UTC().Add(-time.Duration(maxWindow*2) * time.Second)
	minCutoff := time.Now().UTC().Add(-5 * time.Minute)
	if cutoff.After(minCutoff) {
		cutoff = minCutoff
	}

	result, err := p.db.Exec(ctx, plugin.Rebind(`
		DELETE FROM rate_counter WHERE window_start < $1
	`, p.dialect), cutoff)
	if err != nil {
		p.logger.Error("rate-limiter: prune counters", "error", err)
	} else if result > 0 {
		p.logger.Debug("rate-limiter: pruned counters", "count", result)
	}
}
