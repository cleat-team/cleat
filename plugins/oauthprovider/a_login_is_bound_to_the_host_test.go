package oauthprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// recordingHostResolver answers with a fixed verdict and keeps what it was
// asked, so a test can assert the handler consulted it about the RIGHT host and
// the RIGHT tenant -- not merely that the response was a refusal. A resolver
// that is never called and a resolver that is called with the wrong arguments
// both produce whatever status the fixture was built to expect.
type recordingHostResolver struct {
	bound bool
	err   error

	hosts []string
	wants []uuid.UUID
}

func (f *recordingHostResolver) TenantForHost(_ context.Context, hostname string, want uuid.UUID) (bool, error) {
	f.hosts = append(f.hosts, hostname)
	f.wants = append(f.wants, want)
	return f.bound, f.err
}

// TestLoginIsBoundToTheHost covers the Host-bound tenant check handleLogin
// performs for itself (cleat#2340).
//
// The check exists because /login is auth-exempt -- an anonymous browser starts
// there, so it carries no credential for auth.HostBindingMiddlewareWithMux to
// bind a tenant from, which is exactly why cleat#2319 put it in
// pluginAuthExemptPatterns. The middleware skips precisely the routes auth
// exempts, so without this check --require-host-match protects every route
// except the one that mints the credential, and ?tenant_id=<victim> is accepted
// from a host bound to a different tenant.
//
// Five cases, and the ones that are NOT refusals are the ones a "does it refuse
// a bad host" test would miss:
//
//   - the check must PASS when the host does own the tenant, or a handler that
//     refused everything unconditionally would satisfy every other case here;
//   - host binding ON with no resolver must refuse, rather than reading "cannot
//     check" as "check passed";
//   - host binding OFF must leave the ?tenant_id=-only path exactly as it was,
//     which is what makes this a conditional rather than a new requirement.
//
// Assertions are on the response BODY, not only the status. The statuses this
// test expects are also reachable from neighbouring checks in handleLogin --
// 400 for a missing or unparseable tenant_id, 500 for a getConfig failure -- so
// a status-only assertion can go green against a branch that is not the one
// under test. CLAUDE.md's "condition that never decides anything" case.
func TestLoginIsBoundToTheHost(t *testing.T) {
	const (
		loginPath = "/oauth/google/login?tenant_id="
		// Deliberately mixed case and carrying a port: NormalizeHost strips the
		// port and lowercases, and tenant_domains rows are written the same way
		// (its doc comment: a row that cannot match a normalised lookup
		// presents as "my domain is configured and cleat refuses it"). Passing
		// the raw header straight through would work in a test whose resolver
		// accepts anything and fail in production, so the assertion below pins
		// the normalisation rather than the presence of a call.
		rawHost    = "Tenant-A.Example.Test:8443"
		wantHost   = "tenant-a.example.test"
		redirectTo = "http://localhost/callback"
	)

	// boundFixture builds the plugin and store a login needs to get as far as a
	// redirect, so that "refused" and "proceeded" are distinguishable.
	newFixture := func(t *testing.T) (*Plugin, http.Handler, *fakeDBStore) {
		t.Helper()
		store := newFakeDBStore()
		p, handler := setupTestPlugin(t, store)
		store.AddOAuthConfig(testTenantID, "google", "g-client-id", "g-secret", redirectTo, "", true)
		return p, handler, store
	}

	doLogin := func(handler http.Handler) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", loginPath+testTenantID.String(), nil)
		req.Host = rawHost
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	t.Run("a tenant that does not own the host is refused before the redirect", func(t *testing.T) {
		p, handler, _ := newFixture(t)
		resolver := &recordingHostResolver{bound: false}
		p.requireHostMatch = true
		p.hostResolver = resolver

		rec := doLogin(handler)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400 for a tenant that does not own the host, got %d: %s",
				rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "does not match the requested host") {
			t.Errorf("400 body %q does not carry the host-binding refusal -- a 400 from the "+
				"tenant_id parse would satisfy a status-only check", body)
		}
		// The refusal must land BEFORE the redirect is issued: a 400 carrying a
		// Location header would already have handed the browser to the provider
		// with the victim tenant's client_id in the query string.
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("refused login still set Location: %q -- the check must run before the "+
				"authorize URL is built, not after", loc)
		}
		if len(resolver.hosts) != 1 {
			t.Fatalf("resolver consulted %d time(s), want exactly 1", len(resolver.hosts))
		}
		if got := resolver.hosts[0]; got != wantHost {
			t.Errorf("resolver asked about host %q, want %q (port stripped, lowercased, from %q)",
				got, wantHost, rawHost)
		}
		if got := resolver.wants[0]; got != testTenantID {
			t.Errorf("resolver asked about tenant %v, want the ?tenant_id= value %v", got, testTenantID)
		}
	})

	t.Run("the host that owns the tenant proceeds to the provider", func(t *testing.T) {
		p, handler, _ := newFixture(t)
		resolver := &recordingHostResolver{bound: true}
		p.requireHostMatch = true
		p.hostResolver = resolver

		rec := doLogin(handler)

		if rec.Code != http.StatusFound {
			t.Fatalf("want 302 when the host owns the tenant, got %d: %s -- a handler that "+
				"refused every host would pass the refusal cases above",
				rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "accounts.google.com") {
			t.Errorf("Location %q is not the provider authorize URL", loc)
		}
		if len(resolver.hosts) != 1 || resolver.hosts[0] != wantHost {
			t.Errorf("resolver saw %v, want exactly one lookup for %q", resolver.hosts, wantHost)
		}
	})

	t.Run("host binding on with no resolver refuses rather than skipping the check", func(t *testing.T) {
		p, handler, _ := newFixture(t)
		p.requireHostMatch = true
		p.hostResolver = nil

		rec := doLogin(handler)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("want 500 when --require-host-match is set but nothing can resolve hosts, "+
				"got %d: %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "misconfigured") {
			t.Errorf("500 body %q does not name the misconfiguration", body)
		}
		// The dangerous alternative reading is "no resolver, so there is nothing
		// to check against, so proceed" -- which is the shape where the flag
		// silently stops protecting anything. Assert the redirect did NOT happen,
		// because 500-with-a-Location would be a refusal that already leaked.
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("unresolvable host binding still set Location: %q", loc)
		}
	})

	t.Run("a resolver error refuses rather than reading as bound", func(t *testing.T) {
		p, handler, _ := newFixture(t)
		p.requireHostMatch = true
		p.hostResolver = &recordingHostResolver{err: errors.New("domain store unreachable")}

		rec := doLogin(handler)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("want 500 when the resolver fails, got %d: %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "host binding check failed") {
			t.Errorf("500 body %q does not name the failed check", body)
		}
	})

	t.Run("host binding off leaves the tenant_id-only path alone", func(t *testing.T) {
		p, handler, _ := newFixture(t)
		// A resolver that would refuse, wired but not required: this is what
		// separates "the branch is conditional" from "the branch is not there,
		// and the test never noticed".
		resolver := &recordingHostResolver{bound: false}
		p.requireHostMatch = false
		p.hostResolver = resolver

		rec := doLogin(handler)

		if rec.Code != http.StatusFound {
			t.Fatalf("want 302 with host binding off, got %d: %s -- the check must be "+
				"conditional on --require-host-match, not unconditional",
				rec.Code, rec.Body.String())
		}
		if len(resolver.hosts) != 0 {
			t.Errorf("resolver consulted %v with host binding off; the check must not run",
				resolver.hosts)
		}
	})
}
