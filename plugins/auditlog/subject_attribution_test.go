package auditlog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#1881. auditlog wrote the empty string into user_id on every request,
// while oauthprovider already resolved a session identity into context and
// nothing read it. This is the read half, tested at the point that actually
// matters: what Middleware enqueues, not what a full database round trip
// happens to contain.
func TestMiddlewareRecordsTheSubjectFromContext(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{}); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := p.Middleware(base)

	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithSubject(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	select {
	case evt := <-p.buffer:
		if evt.userID != "alice@example.com" {
			t.Errorf("userID = %q, want %q: the subject in context did not reach the queued "+
				"event", evt.userID, "alice@example.com")
		}
	case <-time.After(time.Second):
		t.Fatal("no event was enqueued")
	}
}

// TestMiddlewareRecordsEmptyUserIDWithoutErroringWhenUnauthenticated is the
// acceptance criterion the issue states explicitly: audit must not become
// something that can refuse traffic. No subject in context is the ordinary
// case for an unauthenticated request, and it must produce an empty user_id,
// not an error and not a dropped event.
func TestMiddlewareRecordsEmptyUserIDWithoutErroringWhenUnauthenticated(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{}); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := p.Middleware(base)

	req := httptest.NewRequest("GET", "/test", nil) // no auth.WithSubject at all
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("an unauthenticated request through audit-log's middleware got status %d, "+
			"want 200: audit-log must never refuse a request over a missing subject", rec.Code)
	}

	select {
	case evt := <-p.buffer:
		if evt.userID != "" {
			t.Errorf("userID = %q, want empty: nothing set a subject for this request", evt.userID)
		}
	case <-time.After(time.Second):
		t.Fatal("no event was enqueued")
	}
}
