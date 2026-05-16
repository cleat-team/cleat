package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/auth"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

func TestIPRateLimiter_Allow(t *testing.T) {
	rl := newIPRateLimiter(10, 5)
	defer rl.stop()

	// Should allow requests within burst.
	for i := 0; i < 5; i++ {
		if !rl.allow("192.168.1.1") {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}

	// Next request should be denied (no tokens left).
	if rl.allow("192.168.1.1") {
		t.Fatal("request should be denied after burst exhausted")
	}
}

func TestIPRateLimiter_SeparateIPs(t *testing.T) {
	rl := newIPRateLimiter(10, 5)
	defer rl.stop()

	// Fill up IP A's bucket but leave IP B untouched.
	for i := 0; i < 5; i++ {
		rl.allow("10.0.0.1")
	}

	// IP A should be exhausted.
	if rl.allow("10.0.0.1") {
		t.Error("IP A should be exhausted")
	}

	// IP B should still work.
	for i := 0; i < 5; i++ {
		if !rl.allow("10.0.0.2") {
			t.Fatalf("IP B request %d should be allowed", i)
		}
	}
}

func TestKeyedRateLimiter_Allow(t *testing.T) {
	kl := newKeyedRateLimiter()
	defer kl.stop()

	key := uuid.New().String()

	// Burst of 3 at rate 10.
	for i := 0; i < 3; i++ {
		if !kl.allow(key, 10, 3) {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}

	// Next request should be denied.
	if kl.allow(key, 10, 3) {
		t.Fatal("request should be denied after burst exhausted")
	}
}

func TestKeyedRateLimiter_SeparateKeys(t *testing.T) {
	kl := newKeyedRateLimiter()
	defer kl.stop()

	keyA := uuid.New().String()
	keyB := uuid.New().String()

	// Exhaust key A.
	for i := 0; i < 2; i++ {
		kl.allow(keyA, 10, 2)
	}
	if kl.allow(keyA, 10, 2) {
		t.Error("key A should be exhausted")
	}

	// Key B should still work.
	if !kl.allow(keyB, 10, 2) {
		t.Error("key B should still be allowed")
	}
}

func TestKeyedRateLimiter_ZeroRate(t *testing.T) {
	kl := newKeyedRateLimiter()
	defer kl.stop()

	// rate=0, burst=100 means 100 initial tokens with no refill.
	// This is not used by the middleware (which skips when tenantRate==0),
	// but verifies the limiter allows burst with zero refill rate.
	for i := 0; i < 100; i++ {
		if !kl.allow("unlimited", 0, 100) {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	// Request 101 should be denied.
	if kl.allow("unlimited", 0, 100) {
		t.Fatal("request beyond burst should be denied")
	}
}

func TestRateLimitMiddleware_IPExceeded(t *testing.T) {
	ipLim := newIPRateLimiter(1000, 1) // burst of 1
	defer ipLim.stop()
	tenantLim := newKeyedRateLimiter()
	defer tenantLim.stop()

	// Use a handler that records whether it was called.
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := rateLimitMiddleware(ipLim, tenantLim, 0, 0)(next)

	// First request: allowed (uses the burst token).
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Fatal("first request should be allowed")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Second request: denied (no tokens left, rate is 1000 req/s → refill takes ~1ms).
	// We need to consume fast enough that refill doesn't help. Use very low rate.
	ipLim2 := newIPRateLimiter(0.001, 1) // 0.001 req/s — effectively no refill
	defer ipLim2.stop()
	handler2 := rateLimitMiddleware(ipLim2, tenantLim, 0, 0)(next)

	called = false
	handler2.ServeHTTP(w, req) // first (burst)
	if !called {
		t.Fatal("burst request should be allowed")
	}

	called = false
	w = httptest.NewRecorder()
	handler2.ServeHTTP(w, req) // second (should be denied)
	if called {
		t.Error("second request should be denied")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("expected JSON error body for 429")
	}
}

func TestRateLimitMiddleware_TenantExceeded(t *testing.T) {
	ipLim := newIPRateLimiter(1000, 100) // generous IP limit
	defer ipLim.stop()
	tenantLim := newKeyedRateLimiter()
	defer tenantLim.stop()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	tid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	// Per-tenant: 1 burst, low rate.
	handler := rateLimitMiddleware(ipLim, tenantLim, rate.Limit(0.001), 1)(next)

	// Request with tenant ID set.
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	ctx := auth.WithTenantID(req.Context(), tid)
	req = req.WithContext(ctx)

	// First request: burst allowed.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !called {
		t.Fatal("first tenant request should be allowed")
	}

	// Second request: denied (tenant rate limit).
	called = false
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if called {
		t.Error("second tenant request should be denied")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
	if body := w.Body.String(); body == "" {
		t.Error("expected JSON error body")
	}
}

func TestRateLimitMiddleware_TenantIndependent(t *testing.T) {
	ipLim := newIPRateLimiter(1000, 100)
	defer ipLim.stop()
	tenantLim := newKeyedRateLimiter()
	defer tenantLim.stop()

	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	})

	tidA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	tidB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	handler := rateLimitMiddleware(ipLim, tenantLim, rate.Limit(0.001), 1)(next)

	// Exhaust tenant A's burst.
	reqA := httptest.NewRequest("GET", "/api/test", nil)
	reqA.RemoteAddr = "10.0.0.1:12345"
	reqA = reqA.WithContext(auth.WithTenantID(reqA.Context(), tidA))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, reqA)
	if calls != 1 {
		t.Fatalf("tenant A burst request should be allowed, calls=%d", calls)
	}

	// Tenant A should now be denied.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, reqA)
	if calls != 1 {
		t.Errorf("tenant A second request should be denied, calls=%d", calls)
	}

	// Tenant B should still work.
	reqB := httptest.NewRequest("GET", "/api/test", nil)
	reqB.RemoteAddr = "10.0.0.1:12345"
	reqB = reqB.WithContext(auth.WithTenantID(reqB.Context(), tidB))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, reqB)
	if calls != 2 {
		t.Errorf("tenant B request should be allowed, calls=%d", calls)
	}
}

func TestRateLimitMiddleware_TenantDisabledWhenZero(t *testing.T) {
	ipLim := newIPRateLimiter(1000, 100)
	defer ipLim.stop()
	tenantLim := newKeyedRateLimiter()
	defer tenantLim.stop()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	// tenantRate = 0 means per-tenant limiting is disabled.
	handler := rateLimitMiddleware(ipLim, tenantLim, 0, 0)(next)

	tid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req = req.WithContext(auth.WithTenantID(req.Context(), tid))

	// Many requests should all be allowed (only IP limit applies).
	for i := 0; i < 50; i++ {
		called = false
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if !called {
			t.Errorf("request %d should be allowed when tenant limit is disabled", i)
			break
		}
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xFwdFor    string
		want       string
	}{
		{"simple", "1.2.3.4:12345", "", "1.2.3.4"},
		{"x-forwarded-for", "10.0.0.1:9999", "1.2.3.4", "1.2.3.4"},
		{"x-forwarded-for multi", "10.0.0.1:9999", "1.2.3.4, 5.6.7.8", "1.2.3.4"},
		{"no port", "1.2.3.4", "", "1.2.3.4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xFwdFor != "" {
				req.Header.Set("X-Forwarded-For", tt.xFwdFor)
			}
			got := clientIP(req)
			if got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWrite429(t *testing.T) {
	w := httptest.NewRecorder()
	write429(w, "test limit hit")

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("expected Retry-After: 1, got %q", w.Header().Get("Retry-After"))
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type: application/json, got %q", w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if body == "" || body == "null\n" {
		t.Error("expected non-empty JSON body")
	}
}

func TestRateLimitMiddleware_NoTenantContext(t *testing.T) {
	ipLim := newIPRateLimiter(1000, 100)
	defer ipLim.stop()
	tenantLim := newKeyedRateLimiter()
	defer tenantLim.stop()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	// Tenant rate limiting enabled but no tenant in context.
	handler := rateLimitMiddleware(ipLim, tenantLim, rate.Limit(1), 1)(next)

	req := httptest.NewRequest("GET", "/api/healthz", nil)
	req.RemoteAddr = "10.0.0.1:12345"

	// Many requests should pass (only IP limit checks; tenant check skipped).
	for i := 0; i < 50; i++ {
		called = false
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if !called {
			t.Errorf("request %d should pass without tenant context", i)
			break
		}
	}
}

func TestKeyedRateLimiter_Cleanup(t *testing.T) {
	kl := newKeyedRateLimiter()
	defer kl.stop()

	key := uuid.New().String()

	// Use the key once.
	if !kl.allow(key, 10, 5) {
		t.Fatal("first request should be allowed")
	}

	kl.mu.Lock()
	_, exists := kl.limits[key]
	kl.mu.Unlock()
	if !exists {
		t.Fatal("entry should exist immediately after use")
	}

	// Force cleanup by setting lastUsed far in the past.
	kl.mu.Lock()
	kl.limits[key].lastUsed = time.Now().Add(-2 * time.Hour)
	kl.mu.Unlock()

	// Simulate cleanup.
	kl.mu.Lock()
	now := time.Now()
	for k, entry := range kl.limits {
		if now.Sub(entry.lastUsed) > time.Hour {
			delete(kl.limits, k)
		}
	}
	kl.mu.Unlock()

	kl.mu.Lock()
	_, exists = kl.limits[key]
	kl.mu.Unlock()
	if exists {
		t.Error("stale entry should have been cleaned up")
	}
}
