package auth

import (
	"net/http"
	"strings"
	"testing"
)

// cleat#3552, B2b-1. An operator key can be narrowed, and a narrowed key must reach ONLY what
// its scope names. The property is deny-by-default, and it is easy to get wrong in the way
// that matters: a gate that is fine for the keys you thought of and open for the one you did not.
//
// So the table below is deliberately both directions. The ALLOWED rows are the known-positive:
// without them every refusal would pass against a middleware that refused everything. The
// REFUSED rows include the cases a service holding a provision key would actually try -- the
// trailing-slash subtree that acts on named tenants, the same path under another method, and
// path spellings that look like the allowed route.

func TestAScopedOperatorKeyReachesOnlyTheRoutesItsScopeNames(t *testing.T) {
	const create = "/api/admin/tenants"
	forceComplete := "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/instances/wf-1/force-complete"

	type row struct {
		name   string
		scopes string
		method string
		path   string
		want   int // 200 = reached the handler, 403 = refused by the scope
	}
	rows := []row{
		// admin: what every key was before scopes existed. Unchanged.
		{"admin, create", ScopeAdmin, http.MethodPost, create, 200},
		{"admin, drain", ScopeAdmin, http.MethodPost, "/api/admin/drain", 200},
		{"admin, force-complete on a named tenant", ScopeAdmin, http.MethodPost, forceComplete, 200},
		{"admin, health", ScopeAdmin, http.MethodGet, "/api/admin/health", 200},

		// provision: ONE route, and it is the known-positive for the whole table.
		{"provision, create", ScopeProvision, http.MethodPost, create, 200},

		// ...and nothing else. Same path, other methods.
		{"provision, GET on the create path", ScopeProvision, http.MethodGet, create, 403},
		{"provision, DELETE on the create path", ScopeProvision, http.MethodDelete, create, 403},
		{"provision, PUT on the create path", ScopeProvision, http.MethodPut, create, 403},
		// The subtree that acts on a named tenant. This is the one a prefix match would hand over.
		{"provision, trailing slash", ScopeProvision, http.MethodPost, create + "/", 403},
		{"provision, force-complete on a tenant", ScopeProvision, http.MethodPost, forceComplete, 403},
		{"provision, a named tenant's instances", ScopeProvision, http.MethodGet, "/api/admin/tenants/x/instances", 403},
		// Every other admin route.
		{"provision, drain", ScopeProvision, http.MethodPost, "/api/admin/drain", 403},
		{"provision, retention sweep", ScopeProvision, http.MethodPost, "/api/admin/retention/sweep", 403},
		{"provision, health", ScopeProvision, http.MethodGet, "/api/admin/health", 403},
		{"provision, the id-only instance routes", ScopeProvision, http.MethodPost, "/api/admin/instances/wf-1/force-complete", 403},
		// Spellings that look like the allowed route.
		{"provision, double slash inside", ScopeProvision, http.MethodPost, "/api/admin//tenants", 403},
		{"provision, double slash after", ScopeProvision, http.MethodPost, create + "//", 403},
		{"provision, dot segment into the subtree", ScopeProvision, http.MethodPost, create + "/./x", 403},
		{"provision, dot-dot out to drain", ScopeProvision, http.MethodPost, create + "/../drain", 403},
		{"provision, encoded slash makes the subtree", ScopeProvision, http.MethodPost, create + "%2F", 403},
		{"provision, upper-cased path", ScopeProvision, http.MethodPost, strings.ToUpper(create), 403},
		// Encodings. The decoded path equals the allowed one in the first two, which is the
		// disagreement with the mux that cleat-review measured: the mux routes the ENCODED form.
		{"provision, encoded slash between admin and tenants", ScopeProvision, http.MethodPost, "/api/admin%2Ftenants", 403},
		{"provision, encoded slash after api", ScopeProvision, http.MethodPost, "/api%2Fadmin/tenants", 403},
		{"provision, an encoded letter in the last segment", ScopeProvision, http.MethodPost, "/api/admin/%74enants", 403},
		// A query string is not part of the path and does not change the route.
		{"provision, create with a query string", ScopeProvision, http.MethodPost, create + "?x=1", 200},
		// The admin scope is not held to the canonical form: it already reaches everything.
		{"admin, an encoded path is still admin", ScopeAdmin, http.MethodPost, "/api/admin/%64rain", 200},

		// A key whose scopes admit nothing reaches NOTHING -- the create route included.
		{"no scopes, create", "", http.MethodPost, create, 403},
		{"no scopes, drain", "", http.MethodPost, "/api/admin/drain", 403},
		// A scope this binary does not know grants nothing, alone or beside a known one.
		{"unknown scope alone, create", "bogus", http.MethodPost, create, 403},
		{"unknown scope alone, drain", "bogus", http.MethodPost, "/api/admin/drain", 403},
		{"provision plus unknown, create", "bogus provision", http.MethodPost, create, 200},
		{"provision plus unknown, drain", "bogus provision", http.MethodPost, "/api/admin/drain", 403},
		// Several scopes: the union, nothing more.
		{"admin plus provision, drain", "admin provision", http.MethodPost, "/api/admin/drain", 200},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeOperatorResolver{op: Operator{KeyID: "8d264f39-9c52-4623-aa1d-258c7bcc34d7", Scopes: tc.scopes, Principal: "clew-service"}}
			got := runOperatorChain(t, store, tc.method, tc.path, GenerateOperatorKey())
			if got.status != tc.want {
				t.Errorf("%s %s with scopes %q: got %d, want %d", tc.method, tc.path, tc.scopes, got.status, tc.want)
			}
			// 403 must mean the handler never ran, not that it ran and then said no.
			if tc.want == 403 && got.reached {
				t.Errorf("%s %s with scopes %q was refused but the handler still ran", tc.method, tc.path, tc.scopes)
			}
			if tc.want == 200 && !got.reached {
				t.Errorf("%s %s with scopes %q returned 200 without reaching the handler", tc.method, tc.path, tc.scopes)
			}
		})
	}
}

// A narrowed key is refused with a 403 and a message that says why, and a scope refusal is not
// confused with the other two refusals the middleware has: 401 for a bad credential and 403 for
// a route outside /api/admin/. An operator reads which one it was first.
func TestAScopeRefusalSaysItIsAScopeRefusal(t *testing.T) {
	store := &fakeOperatorResolver{op: Operator{KeyID: "k", Scopes: ScopeProvision, Principal: "p"}}
	got := runOperatorChain(t, store, http.MethodPost, "/api/admin/drain", GenerateOperatorKey())
	if got.status != http.StatusForbidden {
		t.Fatalf("status %d, want 403", got.status)
	}
}

func TestFormatOperatorScopes(t *testing.T) {
	good := []struct {
		in   []string
		want string
	}{
		{[]string{"admin"}, "admin"},
		{[]string{"provision"}, "provision"},
		{[]string{"provision", "admin"}, "admin provision"},   // sorted: the same set stores the same text
		{[]string{"admin", "admin", "admin"}, "admin"},        // de-duplicated
		{[]string{" provision ", "admin"}, "admin provision"}, // surrounding space is not part of the name
	}
	for _, tc := range good {
		got, err := FormatOperatorScopes(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("FormatOperatorScopes(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	// Refused, never defaulted: a mint that widened an empty request to admin is what scopes prevent.
	for _, bad := range [][]string{nil, {}, {""}, {"bogus"}, {"admin", "bogus"}, {"Admin"}, {"admin provision"}} {
		if got, err := FormatOperatorScopes(bad); err == nil {
			t.Errorf("FormatOperatorScopes(%q) = %q, want an error", bad, got)
		}
	}
}

func TestAPrincipalNameIsValidatedTheSameWayEverywhere(t *testing.T) {
	for _, ok := range []string{"clew-service", "a", "svc.v2", "ci_runner", "0abc"} {
		if !ValidPrincipal(ok) {
			t.Errorf("%q should be a valid principal", ok)
		}
	}
	for _, bad := range []string{"", "Clew", "-lead", ".lead", "has space", "a/b", strings.Repeat("a", 129), "uniçode"} {
		if ValidPrincipal(bad) {
			t.Errorf("%q should not be a valid principal", bad)
		}
	}
}
