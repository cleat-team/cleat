package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoHandler is the inner handler every test below wraps -- it reports
// 200 and nothing else, so a response's CORS headers (or lack of them) are
// the only thing under test, never the body.
var echoHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// TestCORSMiddlewareOffByDefault is cleat#2345's own scope line: an empty
// allowedOrigins must be byte-identical to no middleware at all, not merely
// "permissive". Checked by asserting NO Access-Control-* header is set even
// for an Origin a deployment might plausibly have meant to allow -- there
// is no implicit default-allow fallback hiding behind the "off" state.
func TestCORSMiddlewareOffByDefault(t *testing.T) {
	mw := newCORSMiddleware(nil)
	h := mw(echoHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the inner handler, untouched)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want unset -- off by default means off, "+
			"not permissive", got)
	}
}

// TestCORSMiddlewareAllowedOrigin is cleat#2345's first named test: an
// allowed origin's ordinary (non-preflight) request reaches the inner
// handler AND gets Access-Control-Allow-Origin naming it back -- the header
// every subsequent response, success or failure, needs for the browser to
// let its own JS read the response (see newCORSMiddleware's doc comment).
func TestCORSMiddlewareAllowedOrigin(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://app.example.com"})
	h := mw(echoHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the inner handler ran)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want the matched origin echoed back", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want \"Origin\" -- a cache keyed on anything else would serve one "+
			"origin's CORS headers to another", got)
	}
}

// TestCORSMiddlewareRefusedOrigin is cleat#2345's second named test: an
// origin NOT on the allowlist gets no CORS headers on an ordinary request.
// The inner handler still runs (this is not a server-side block -- see
// newCORSMiddleware's doc comment on why refusing here would also break a
// non-browser caller sending an unrecognised Origin), but without the
// header, a BROWSER's own enforcement is what keeps this origin's page code
// from reading the response.
func TestCORSMiddlewareRefusedOrigin(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://app.example.com"})
	h := mw(echoHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- a non-browser caller with an unrecognised Origin "+
			"must not be server-side blocked", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want unset for a refused origin", got)
	}
}

// TestCORSMiddlewarePreflightAllowedOrigin is cleat#2345's third named
// test (preflight, allowed side): a preflight from a listed origin gets
// 204, the matched origin echoed back, and -- the issue's explicit scope
// line -- Authorization present in Access-Control-Allow-Headers.
func TestCORSMiddlewarePreflightAllowedOrigin(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://app.example.com"})
	h := mw(echoHandler)

	req := httptest.NewRequest(http.MethodOptions, "/api/workflows", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for an allowed preflight", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want the matched origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("Access-Control-Allow-Methods is unset for an allowed preflight")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !containsToken(got, "Authorization") {
		t.Fatalf("Access-Control-Allow-Headers = %q, want it to include Authorization for an "+
			"allowed origin's preflight (cleat#2345's own scope line)", got)
	}
}

// TestCORSMiddlewarePreflightRefusedOrigin is cleat#2345's third named test
// (preflight, refused side): a preflight from an origin NOT on the
// allowlist is refused outright -- no 204, no Access-Control-Allow-Origin,
// and critically NO Access-Control-Allow-Headers at all, so Authorization
// never reaches a disallowed origin's preflight response either.
func TestCORSMiddlewarePreflightRefusedOrigin(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://app.example.com"})
	h := mw(echoHandler)

	req := httptest.NewRequest(http.MethodOptions, "/api/workflows", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a refused preflight", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want unset for a refused preflight", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want unset -- Authorization must never "+
			"reach a disallowed origin's preflight response", got)
	}
}

// TestCORSMiddlewareNeverEchoesWildcard guards the property this file's own
// doc comment states as enforced "by construction": no code path writes
// "*" to Access-Control-Allow-Origin, even when the allowlist itself
// contains a literal "*" entry (a deployment's own config mistake). The
// header's value is always the exact Origin the request carried, matched
// by equality -- "*" in allowedOrigins can only ever match a request whose
// Origin header is the literal string "*", which no real browser sends.
func TestCORSMiddlewareNeverEchoesWildcard(t *testing.T) {
	mw := newCORSMiddleware([]string{"*"})
	h := mw(echoHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want unset -- a literal \"*\" entry in the "+
			"allowlist must not act as a wildcard match against an unrelated real origin", got)
	}
}

// TestCORSMiddlewareVaryOriginIsUnconditional guards cleat-review's finding
// on #3282: the response varies by Origin on every path through this
// middleware, including the two that set no Access-Control-* header at all
// (a refused preflight's 403, and an ordinary request from a disallowed or
// absent origin) -- so Vary: Origin must be set there too, or an
// intermediary cache sitting in front of the worker's API could serve one
// origin's cached response to a later request from a different origin for
// the same URL. Covers the disallowed-preflight and disallowed-ordinary
// branches; the two allowed branches are already covered by
// TestCORSMiddlewareAllowedOrigin and TestCORSMiddlewarePreflightAllowedOrigin,
// which assert the same header.
func TestCORSMiddlewareVaryOriginIsUnconditional(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://app.example.com"})
	h := mw(echoHandler)

	t.Run("ordinary disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Fatalf("Vary = %q, want %q -- an intermediary cache must not treat this "+
				"disallowed-origin response as Origin-independent", got, "Origin")
		}
	})

	t.Run("ordinary absent origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Fatalf("Vary = %q, want %q -- a request with no Origin header still varies "+
				"by Origin (an absent header is a distinct case from a disallowed one)", got, "Origin")
		}
	})

	t.Run("refused preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/workflows", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 -- precondition for this subtest", rec.Code)
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Fatalf("Vary = %q, want %q -- a cached 403 for one origin must not be served "+
				"to a later preflight from a different origin for the same URL", got, "Origin")
		}
	})
}

// containsToken reports whether value, read as a comma-separated header
// list, contains token exactly (case-sensitive, matching this file's own
// equality-only matching elsewhere) -- not merely as a substring, so
// "Authorization" would not be fooled by a hypothetical
// "X-Not-Authorization" token.
func containsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) == token {
			return true
		}
	}
	return false
}
