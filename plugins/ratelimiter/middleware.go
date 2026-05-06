package ratelimiter

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/auth"
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
		info := p.allow(tid)
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
