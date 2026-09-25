package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type fakeDomains struct {
	owner map[string]uuid.UUID
	err   error
	calls int
}

func (f *fakeDomains) TenantForHost(_ context.Context, hostname string, want uuid.UUID) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	owner, ok := f.owner[hostname]
	return ok && owner == want, nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
}

func TestHostBindingRefusesAHostTheTenantDoesNotOwn(t *testing.T) {
	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := &fakeDomains{owner: map[string]uuid.UUID{
		"a.example.com": tenantA,
		"b.example.com": tenantB,
	}}
	h := HostBindingMiddlewareWithMux(res, nil)(okHandler())

	for _, tc := range []struct {
		name string
		host string
		want int
	}{
		{"own host is allowed", "a.example.com", http.StatusOK},
		{"own host with a port is allowed", "a.example.com:8080", http.StatusOK},
		{"uppercase host is normalised and allowed", "A.Example.COM", http.StatusOK},
		{"another tenant's host is refused", "b.example.com", http.StatusNotFound},
		{"an unregistered host is refused", "nobody.example.com", http.StatusNotFound},
		{"an empty host is refused", "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
			r.Host = tc.host
			r = r.WithContext(WithTenantID(context.Background(), tenantA))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("Host %q: got %d, want %d", tc.host, w.Code, tc.want)
			}
		})
	}
}

// TestHostBindingIsNotAnOracle is the assertion the design turns on, and it is
// the one a later refactor is most likely to break by "improving" an error
// message.
//
// If a foreign hostname and an unregistered one produced different responses,
// any tenant could enumerate which hostnames -- and therefore which tenants --
// exist, using only its own valid credential. Comparing status codes alone
// would miss a body that differs, so this compares both.
func TestHostBindingIsNotAnOracle(t *testing.T) {
	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := &fakeDomains{owner: map[string]uuid.UUID{"taken.example.com": tenantB}}
	h := HostBindingMiddlewareWithMux(res, nil)(okHandler())

	get := func(host string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
		r.Host = host
		r = r.WithContext(WithTenantID(context.Background(), tenantA))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	foreignCode, foreignBody := get("taken.example.com")
	unknownCode, unknownBody := get("free.example.com")

	if foreignCode != unknownCode || foreignBody != unknownBody {
		t.Errorf("a foreign host and an unregistered host are distinguishable:\n"+
			"  foreign:  %d %q\n  unknown:  %d %q\n\n"+
			"That difference lets any tenant enumerate which hostnames exist "+
			"using its own valid credential.",
			foreignCode, foreignBody, unknownCode, unknownBody)
	}
	if foreignBody == "" || foreignCode == http.StatusOK {
		t.Fatalf("UNMEASURED: both cases returned %d %q, so this test would pass "+
			"against a middleware that refused nothing", foreignCode, foreignBody)
	}
}

// A database error must not become a way through. The rate limiter fails open
// on purpose because its job is availability-shaped; this one's is not.
func TestHostBindingFailsClosedWhenTheLookupErrors(t *testing.T) {
	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := &fakeDomains{err: errors.New("database is down")}
	h := HostBindingMiddlewareWithMux(res, nil)(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	r.Host = "a.example.com"
	r = r.WithContext(WithTenantID(context.Background(), tenantA))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusOK {
		t.Error("a lookup error let the request through; this middleware must fail closed")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503 so the caller can tell 'unavailable' from 'refused'", w.Code)
	}
}

func TestHostBindingSkipsPathsWithNoTenantUrlToPresent(t *testing.T) {
	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := &fakeDomains{owner: map[string]uuid.UUID{}}
	h := HostBindingMiddlewareWithMux(res, nil, "POST /ingest/{source_id}")(okHandler())

	for _, tc := range []struct{ name, method, path string }{
		{"healthz", http.MethodGet, "/healthz"},
		{"livez", http.MethodGet, "/livez"},
		{"readyz", http.MethodGet, "/readyz"},
		{"metrics", http.MethodGet, "/metrics"},
		{"a public pattern", http.MethodPost, "/ingest/stripe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Host = "unregistered.example.com"
			r = r.WithContext(WithTenantID(context.Background(), tenantA))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("%s %s was refused (%d); it is addressed by infrastructure or by "+
					"a party with no cleat credential, so it has no tenant URL to present",
					tc.method, tc.path, w.Code)
			}
		})
	}
}

// With no tenant in context the middleware passes through, and the reason is
// worth asserting rather than leaving to the doc comment: under --require-auth
// such a request was already answered 401 upstream, so refusing here would only
// change behaviour for --require-auth=false deployments.
func TestHostBindingPassesThroughWhenThereIsNoTenant(t *testing.T) {
	res := &fakeDomains{owner: map[string]uuid.UUID{}}
	h := HostBindingMiddlewareWithMux(res, nil)(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	r.Host = "unregistered.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if res.calls != 0 {
		t.Errorf("the resolver was consulted %d times with no tenant to compare against", res.calls)
	}
}

func TestHostBindingMiddlewareWithMux_LiteralSiblingOfPublicWildcardStaysProtected(t *testing.T) {
	// Same defect as MiddlewareWithMux's sibling test (cleat#2274), one layer
	// in: HostBindingMiddleware shares buildPublicMatcher, so it independently
	// treats POST /ingest/sources as exempt from the host check too, even for
	// a request that already carries a resolved tenant (i.e. one that made it
	// past a since-fixed auth.MiddlewareWithMux). Given the real mux both patterns
	// are registered on, it must not.
	tenantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := &fakeDomains{owner: map[string]uuid.UUID{"a.example.com": tenantA}}

	mux := http.NewServeMux()
	mux.Handle("POST /ingest/{source_id}", okHandler())
	mux.Handle("POST /ingest/sources", okHandler())

	h := HostBindingMiddlewareWithMux(res, mux, "POST /ingest/{source_id}")(mux)

	// A tenant-bound request to the literal sibling, on a host that tenant
	// does not own, must be refused -- not waved through as "public".
	r := httptest.NewRequest(http.MethodPost, "/ingest/sources", nil)
	r.Host = "wrong.example.com"
	r = r.WithContext(WithTenantID(context.Background(), tenantA))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("POST /ingest/sources on an unowned host: got %d, want 404 (refused)", w.Code)
	}

	// The actual public wildcard, with no tenant in context (as it would have
	// if auth.MiddlewareWithMux treated it as public and set none), still passes
	// through regardless of Host.
	r = httptest.NewRequest(http.MethodPost, "/ingest/abc-123", nil)
	r.Host = "wrong.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("POST /ingest/{source_id} with no tenant: got %d, want 200 (public)", w.Code)
	}
}

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"example.com", "example.com"},
		{"Example.COM", "example.com"},
		{"example.com:8080", "example.com"},
		{"  example.com:443  ", "example.com"},
		{"[::1]:8080", "[::1]"},
		{"[2001:db8::1]", "[2001:db8::1]"},
		// A bare IPv6 address has colons and no port. Stripping at the last
		// colon would silently mangle it, so the port is only removed when what
		// follows is entirely digits.
		{"2001:db8::1", "2001:db8::1"},
		{"", ""},
	} {
		if got := NormalizeHost(tc.in); got != tc.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
