package ratelimiter

import (
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

// Middleware wraps the next handler with per-tenant rate limiting. It checks
// all rate limits configured for the tenant. If any limit is exceeded, it
// returns 429 Too Many Requests with a Retry-After header.
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := auth.TenantIDFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if p.allow(tid) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limit exceeded"}`))
	})
}

// allow checks whether the tenant has at least one token in all of their
// configured rate limit buckets. It refills all buckets, checks each one,
// and only consumes tokens if every bucket has capacity. Returns true if
// all limits allow the request.
func (p *Plugin) allow(tid uuid.UUID) bool {
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
		return true // no rate limits configured for this tenant
	}

	// Phase 1: refill all buckets based on elapsed time.
	for _, tb := range tbs {
		tb.refill()
	}

	// Phase 2: check that every bucket has at least one token.
	for _, tb := range tbs {
		if tb.tokens < 1 {
			return false
		}
	}

	// Phase 3: consume one token from every bucket.
	for _, tb := range tbs {
		tb.tokens--
	}
	return true
}
