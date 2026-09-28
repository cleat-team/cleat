package ratelimiter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestASpentBucketDoesNotRefuseItsOwnManagementRoutes is the regression test for
// cleat#2551.
//
// THE DEFECT. RegisterRoutes mounts `GET|PUT|DELETE /rate-limits…` and
// Middleware wraps EVERY request for a tenant that has any bucket configured,
// with no path exemption. So once a bucket is spent, `PUT /rate-limits/{key}` —
// the request that would RAISE it — is refused by it, for the rest of the
// window. Measured against a real worker: a limit of 5 per 60s refused both a
// raise to 100000 and a plain GET, and there is no way around it from outside.
//
// A lockout is a worse class than a wrong number: the operator meets it exactly
// when the tenant is being hammered, which is when they need the remedy, and
// `DELETE` — the other remedy — is behind the same bucket.
//
// THE SHAPE OF THE ASSERTION, because it is what makes this fail-able rather
// than decorative. It does not check that the routes are MOUNTED, or that they
// return 200: it first spends the bucket and proves the limiter is ENGAGED by
// getting a 429 on an ordinary path, and only then requires the management
// routes to get through. Without that first step the test would pass against a
// plugin with no limits configured at all — the same green from a check that
// measured nothing.
func TestASpentBucketDoesNotRefuseItsOwnManagementRoutes(t *testing.T) {
	p, handler := authMiddlewareHandler(t, 2, 300)

	// Spend it. `consumeTokens` calls allow() directly until it denies.
	if n := consumeTokens(t, p, testTenantA); n == 0 {
		t.Fatal("consumeTokens consumed nothing, so the limit was never engaged " +
			"and the assertions below would pass vacuously")
	}

	// ESTABLISH THE PRECONDITION: an ordinary route is refused. This is the
	// limiter working, and it is what makes the exemption below meaningful.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("an ordinary request on a spent bucket returned %d, want 429: the "+
			"limiter is not engaged, so nothing below is a test of the exemption",
			rec.Code)
	}

	// THE ASSERTION. Every management route, because all three are remedies:
	// PUT raises the limit, DELETE removes it, GET is how an operator finds out
	// what is configured before choosing between them.
	for _, tc := range []struct{ method, path string }{
		{"GET", managementBasePath},
		{"GET", managementBasePath + "/default"},
		{"PUT", managementBasePath + "/default"},
		{"DELETE", managementBasePath + "/default"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code == http.StatusTooManyRequests {
			t.Errorf("%s %s was refused by the very limit it manages.\n\n"+
				"An operator who has exhausted a tenant's bucket cannot raise it, "+
				"lower it, or even read it back for the rest of the window -- and "+
				"that is the moment they need to (cleat#2551).",
				tc.method, tc.path)
		}
	}
}

// TestTheManagementExemptionStopsAtThePathBoundary is the negative control for
// the exemption, and it is the one that would go unnoticed.
//
// `strings.HasPrefix(path, "/rate-limits")` without a boundary exempts every
// path that merely begins with those characters — `/rate-limits-are-not-a-
// sibling` included. That is a prefix check quietly becoming broader than the
// surface it was written for, and nothing about the positive test above would
// see it: exempting MORE still passes "the management routes get through".
//
// So this sends a path one character outside the boundary and requires it to be
// LIMITED. It fails if the boundary is dropped, and it is the reason the check
// is spelled `path == base || HasPrefix(path, base+"/")`.
func TestTheManagementExemptionStopsAtThePathBoundary(t *testing.T) {
	p, handler := authMiddlewareHandler(t, 2, 300)
	consumeTokens(t, p, testTenantA)

	for _, path := range []string{
		"/rate-limits-are-not-a-sibling",
		"/rate-limit",
		"/rate-limitsx",
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("GET %s returned %d on a spent bucket, want 429: the exemption "+
				"covers a path outside %q, so it is broader than the surface it was "+
				"written for", path, rec.Code, managementBasePath)
		}
	}
}
