package ratelimiter

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

// tokenBucket implements a simple token bucket rate limiter. All operations
// must be serialized by the parent Plugin's mu lock.
type tokenBucket struct {
	maxTokens  float64
	refillRate float64 // tokens per second
	tokens     float64
	lastRefill time.Time
}

// newTokenBucket creates a token bucket initialized with maxTokens and a
// refill rate of maxRequests / windowSeconds tokens per second.
func newTokenBucket(maxRequests, windowSeconds int) *tokenBucket {
	return &tokenBucket{
		maxTokens:  float64(maxRequests),
		refillRate: float64(maxRequests) / float64(windowSeconds),
		tokens:     float64(maxRequests),
		lastRefill: time.Now(),
	}
}

// refill adds tokens based on elapsed time. Tokens are capped at maxTokens.
// Must be called under p.mu.
func (tb *tokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens = math.Min(tb.maxTokens, tb.tokens+elapsed*tb.refillRate)
	tb.lastRefill = now
}

// rateLimitInfo holds the result of a rate limit check, including the
// header values to report to the client.
type rateLimitInfo struct {
	allowed   bool
	remaining float64 // tokens remaining in the most constrained bucket
	limit     float64 // max tokens (burst capacity) of the most constrained bucket
	resetAt   int64   // unix timestamp when the most constrained bucket will be full
}

// rateLimitConfig describes a single rate limit configuration for DB mode.
type rateLimitConfig struct {
	key           string
	maxRequests   int
	windowSeconds int
}

// Middleware wraps the next handler with per-tenant rate limiting. It checks
// all rate limits configured for the tenant. If any limit is exceeded, it
// returns 429 Too Many Requests with standard rate limit headers.
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := auth.TenantIDFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		var info rateLimitInfo
		if p.mode == "db" {
			info = p.allowDB(tid, r)
		} else {
			info = p.allow(tid)
		}

		if info.limit > 0 {
			w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%.0f", info.limit))
			w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%.0f", info.remaining))
		}
		if info.allowed {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", info.resetAt))
		resetIn := time.Until(time.Unix(info.resetAt, 0))
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(resetIn.Seconds())+1))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limit exceeded"}`))
	})
}

// allow checks whether the tenant has at least one token in all of their
// configured rate limit buckets. It refills all buckets, checks each one,
// and only consumes tokens if every bucket has capacity. Returns rate limit
// info including remaining tokens and reset time.
func (p *Plugin) allow(tid uuid.UUID) rateLimitInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	prefix := tid.String() + "/"

	// Collect all buckets for this tenant.
	var tbs []*tokenBucket
	for key, bucket := range p.buckets {
		if strings.HasPrefix(key, prefix) {
			tbs = append(tbs, bucket)
		}
	}
	if len(tbs) == 0 {
		return rateLimitInfo{allowed: true} // no rate limits configured for this tenant
	}

	// Phase 1: refill all buckets based on elapsed time.
	for _, tb := range tbs {
		tb.refill()
	}

	// Phase 2: check that every bucket has at least one token.
	info := rateLimitInfo{allowed: true}
	for _, tb := range tbs {
		if tb.tokens < 1 {
			info.allowed = false
		}
	}

	if info.allowed {
		// Phase 3: consume one token from every bucket.
		for _, tb := range tbs {
			tb.tokens--
		}
	}

	// Compute header values from the most constrained bucket (lowest remaining).
	info.remaining = math.MaxFloat64
	var constrained *tokenBucket
	for _, tb := range tbs {
		if tb.tokens < info.remaining {
			info.remaining = tb.tokens
			constrained = tb
		}
	}
	if constrained != nil {
		info.limit = constrained.maxTokens
		tokensNeeded := constrained.maxTokens - constrained.tokens
		secondsToRefill := tokensNeeded / constrained.refillRate
		info.resetAt = time.Now().Unix() + int64(math.Ceil(secondsToRefill))
	}

	return info
}

// allowDB checks rate limits using the DB-backed sliding-window counter.
// It collects all rate limit configs for the tenant and checks each one
// against the rate_counter table for cross-worker enforcement.
func (p *Plugin) allowDB(tid uuid.UUID, r *http.Request) rateLimitInfo {
	p.mu.Lock()
	prefix := tid.String() + "/"
	var configs []rateLimitConfig
	for key := range p.buckets {
		if strings.HasPrefix(key, prefix) {
			limitKey := strings.TrimPrefix(key, prefix)
			b := p.buckets[key]
			configs = append(configs, rateLimitConfig{
				key:           limitKey,
				maxRequests:   int(b.maxTokens),
				windowSeconds: int(b.maxTokens / b.refillRate),
			})
		}
	}
	p.mu.Unlock()

	if len(configs) == 0 {
		return rateLimitInfo{allowed: true}
	}

	info := rateLimitInfo{allowed: true, remaining: math.MaxFloat64}
	var constrainedCfg rateLimitConfig
	for _, cfg := range configs {
		allowed, remaining, err := p.checkDBRateLimit(r.Context(), tid, cfg)
		if err != nil {
			p.logger.Error("rate-limiter: db check", "error", err)
			return rateLimitInfo{allowed: true} // fail open
		}
		if !allowed {
			info.allowed = false
		}
		if float64(remaining) < info.remaining {
			info.remaining = float64(remaining)
			info.limit = float64(cfg.maxRequests)
			constrainedCfg = cfg
		}
	}

	if info.remaining == math.MaxFloat64 {
		info.remaining = 0
	}
	info.resetAt = time.Now().Unix() + int64(math.Ceil(float64(constrainedCfg.windowSeconds)))
	return info
}

// checkDBRateLimit checks a single rate limit against the DB counter table
// using per-second buckets summed across the configured window (sliding window).
// Three-step approach that works across all supported databases:
//  1. Best-effort INSERT for the current second bucket
//  2. SELECT SUM(count) across all buckets within the sliding window
//  3. If under limit, atomically increment the current second's bucket
func (p *Plugin) checkDBRateLimit(ctx context.Context, tid uuid.UUID, cfg rateLimitConfig) (allowed bool, remaining int, err error) {
	maxRequests := cfg.maxRequests
	if maxRequests <= 0 {
		return false, 0, nil
	}

	windowSeconds := cfg.windowSeconds
	now := time.Now().UTC()
	windowStart := now.Truncate(time.Second) // per-second bucket
	windowCutoff := now.Add(-time.Duration(windowSeconds) * time.Second)

	// Step 1: Best-effort INSERT to create the row for this second if it doesn't exist.
	q := plugin.Rebind(`
		INSERT INTO rate_counter (tenant_id, limit_key, window_start, count)
		VALUES ($1, $2, $3, 0)
	`, p.dialect)
	_, err = p.db.Exec(ctx, q, tid, cfg.key, windowStart)
	if err != nil {
		// Expected duplicate key error when row already exists.
		if !isDuplicateKeyError(err, p.dialect) {
			return false, 0, err
		}
	}

	// Step 2: Read the total count across all buckets in the sliding window.
	q = plugin.Rebind(`
		SELECT COALESCE(SUM(count), 0) FROM rate_counter
		WHERE tenant_id = $1 AND limit_key = $2 AND window_start > $3
	`, p.dialect)
	var currentCount int
	row := p.db.QueryRow(ctx, q, tid, cfg.key, windowCutoff)
	if err := row.Scan(&currentCount); err != nil {
		// Fail open — treat as first request.
		return true, maxRequests - 1, nil
	}

	if currentCount >= maxRequests {
		remaining = maxRequests - currentCount
		if remaining < 0 {
			remaining = 0
		}
		return false, remaining, nil
	}

	// Step 3: Increment the current second's counter.
	q = plugin.Rebind(`
		UPDATE rate_counter SET count = count + 1
		WHERE tenant_id = $1 AND limit_key = $2 AND window_start = $3
	`, p.dialect)
	if _, err := p.db.Exec(ctx, q, tid, cfg.key, windowStart); err != nil {
		return false, 0, err
	}

	remaining = maxRequests - currentCount - 1
	if remaining < 0 {
		remaining = 0
	}
	return true, remaining, nil
}

// isDuplicateKeyError returns true when the error is a primary key or unique
// constraint violation, using dialect-specific error message heuristics.
func isDuplicateKeyError(err error, dialect plugin.Dialect) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch dialect {
	case plugin.DialectPostgres:
		return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "23505")
	case plugin.DialectMySQL:
		return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "1062")
	case plugin.DialectMSSQL:
		return strings.Contains(msg, "PRIMARY KEY") || strings.Contains(msg, "2627")
	}
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "Duplicate")
}
