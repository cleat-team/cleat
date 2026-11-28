package auth

// cleat#3654. MiddlewareWithMux tells the layers inside it that a request matched an exempt PATTERN, so
// oauthprovider.Middleware can leave such a request alone. A flag that means "skip" is an auth-bypass shape,
// so what is pinned here is that it is set by the mux's answer and by nothing else: the lookalikes of an
// exempt route must NOT carry it and must still be gated.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestMatchedExemptRouteIsSetByTheMatchedPatternOnly(t *testing.T) {
	store := newFakeDBStore()
	addTestKey(store)
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	// The real shape: the exempt wildcard AND a literal sibling that must win, both on one serving mux.
	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux.Handle("POST /ingest/{source_id}", ok)
	mux.Handle("POST /ingest/sources", ok)
	mux.Handle("POST /slack/interactive", ok)
	mux.Handle("GET /api/keys", ok)

	var reached, flagged bool
	h := MiddlewareWithMux(engine.NewPostgresStore(db), true, mux,
		"POST /ingest/{source_id}", "POST /slack/interactive")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached, flagged = true, MatchedExemptRoute(r.Context())
		}))

	for _, tc := range []struct {
		name, method, target string
		exempt               bool
	}{
		// CONTROLS: the two exempt patterns are flagged and reach the handler with no credential.
		{"the exempt wildcard", "POST", "/ingest/abc-123", true},
		{"the exempt literal", "POST", "/slack/interactive", true},

		// LOOKALIKES: each must be refused 401 and carry no flag.
		{"a literal sibling of the wildcard", "POST", "/ingest/sources", false},
		{"a longer prefix", "POST", "/ingestx", false},
		{"the prefix with no id", "POST", "/ingest", false},
		{"the prefix with an empty id", "POST", "/ingest/", false},
		{"a trailing slash after the id", "POST", "/ingest/abc/", false},
		{"a deeper path under an id", "POST", "/ingest/abc/def", false},
		{"dot-dot out of the exempt route", "POST", "/ingest/a/../../api/keys", false},
		{"an encoded slash in the id", "POST", "/ingest/a%2f..%2f..%2fapi%2fkeys", true}, // one segment: it IS the wildcard
		{"the wrong method on the wildcard", "GET", "/ingest/abc-123", false},
		{"the wrong method on the literal", "GET", "/slack/interactive", false},
		{"a trailing slash on the literal", "POST", "/slack/interactive/", false},
		{"an authenticated route", "GET", "/api/keys", false},
	} {
		reached, flagged = false, false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.target, nil))
		if tc.exempt {
			if !reached || !flagged || w.Code != http.StatusOK {
				t.Errorf("%s: %s %s reached=%v flagged=%v code=%d; want reached, flagged, 200", tc.name, tc.method, tc.target, reached, flagged, w.Code)
			}
			continue
		}
		if reached || flagged || w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %s %s reached=%v flagged=%v code=%d; want gated: not reached, not flagged, 401", tc.name, tc.method, tc.target, reached, flagged, w.Code)
		}
	}

	// An AUTHENTICATED request reaches the handler too, and must not be flagged: the flag says "core auth
	// waived this route", and for this one it did not. A flag set on every request that gets through would
	// turn the plugin's session check off for every route.
	reached, flagged = false, false
	req := httptest.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !reached || flagged || w.Code != http.StatusOK {
		t.Errorf("a valid key on an authenticated route: reached=%v flagged=%v code=%d; want reached, NOT flagged, 200", reached, flagged, w.Code)
	}
}

// A context nothing set is not an exempt route, and neither is an infrastructure path: those are answered
// without authentication but were not exempted by pattern.
func TestMatchedExemptRouteIsFalseUnlessMiddlewareWithMuxSetIt(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	if MatchedExemptRoute(httptest.NewRequest("GET", "/", nil).Context()) {
		t.Fatal("a bare request context reads as an exempt route")
	}
	var flagged bool
	h := MiddlewareWithMux(engine.NewPostgresStore(db), true, http.NewServeMux(), "POST /ingest/{source_id}")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { flagged = MatchedExemptRoute(r.Context()) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if flagged {
		t.Error("/healthz is an infrastructure path, not an exempted pattern, and must not carry the flag")
	}
}
