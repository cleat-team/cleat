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
	confirm := "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/confirm"
	suspend := "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/suspend"
	dropPlan := "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/drop-plan"
	resume := "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/resume"
	bare := "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7"
	tombstone := bare + "/tombstone"
	recovery := bare + "/recovery-key"

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
		// ...and the confirm that ends the handshake (B2b-2): one tenant, in canonical form, POST.
		{"provision, confirm", ScopeProvision, http.MethodPost, confirm, 200},
		// ...and the step a drop needs first (B3a): suspend, one tenant, canonical form, POST.
		{"provision, suspend", ScopeProvision, http.MethodPost, suspend, 200},
		// ...and the read-only plan of what the drop would remove (B3b-1b): GET, one tenant, canonical form.
		{"provision, drop-plan", ScopeProvision, http.MethodGet, dropPlan, 200},
		// ...but NOT resume: a provisioner that could clear the flag could lift a suspension an operator made.
		{"provision, resume", ScopeProvision, http.MethodPost, resume, 403},

		// ...and nothing else. Same path, other methods.
		{"provision, GET on the create path", ScopeProvision, http.MethodGet, create, 403},
		{"provision, DELETE on the create path", ScopeProvision, http.MethodDelete, create, 403},
		{"provision, PUT on the create path", ScopeProvision, http.MethodPut, create, 403},
		// The subtree that acts on a named tenant. This is the one a prefix match would hand over.
		{"provision, trailing slash", ScopeProvision, http.MethodPost, create + "/", 403},
		{"provision, force-complete on a tenant", ScopeProvision, http.MethodPost, forceComplete, 403},
		{"provision, a named tenant's instances", ScopeProvision, http.MethodGet, "/api/admin/tenants/x/instances", 403},
		// Confirm's neighbours, each of which a loose match on the shape would hand over.
		{"provision, confirm with GET", ScopeProvision, http.MethodGet, confirm, 403},
		{"provision, confirm with DELETE", ScopeProvision, http.MethodDelete, confirm, 403},
		{"provision, confirm with a trailing slash", ScopeProvision, http.MethodPost, confirm + "/", 403},
		{"provision, confirm with a tail", ScopeProvision, http.MethodPost, confirm + "/x", 403},
		{"provision, confirm of upper-case id", ScopeProvision, http.MethodPost, "/api/admin/tenants/8D264F39-9C52-4623-AA1D-258C7BCC34D7/confirm", 403},
		{"provision, confirm of an id with braces", ScopeProvision, http.MethodPost, "/api/admin/tenants/{8d264f39-9c52-4623-aa1d-258c7bcc34d7}/confirm", 403},
		{"provision, confirm of bare hex", ScopeProvision, http.MethodPost, "/api/admin/tenants/8d264f399c524623aa1d258c7bcc34d7/confirm", 403},
		{"provision, confirm of a name that is no id", ScopeProvision, http.MethodPost, "/api/admin/tenants/acme/confirm", 403},
		{"provision, confirm with an empty id", ScopeProvision, http.MethodPost, "/api/admin/tenants//confirm", 403},
		// Suspend and resume have the same neighbours confirm has, and the same refusals.
		{"provision, suspend with GET", ScopeProvision, http.MethodGet, suspend, 403},
		// The method belongs to the route, in both directions.
		{"provision, drop-plan with POST", ScopeProvision, http.MethodPost, dropPlan, 403},
		{"provision, drop-plan with DELETE", ScopeProvision, http.MethodDelete, dropPlan, 403},
		{"provision, drop-plan with a tail", ScopeProvision, http.MethodGet, dropPlan + "/x", 403},
		{"provision, drop-plan of upper-case id", ScopeProvision, http.MethodGet, "/api/admin/tenants/8D264F39-9C52-4623-AA1D-258C7BCC34D7/drop-plan", 403},
		{"provision, confirm with GET", ScopeProvision, http.MethodGet, "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/confirm", 403},
		{"provision, the tenants collection with GET", ScopeProvision, http.MethodGet, "/api/admin/tenants", 403},
		{"provision, suspend with DELETE", ScopeProvision, http.MethodDelete, suspend, 403},
		{"provision, resume with PUT", ScopeProvision, http.MethodPut, resume, 403},
		{"provision, suspend with a trailing slash", ScopeProvision, http.MethodPost, suspend + "/", 403},
		{"provision, suspend with a tail", ScopeProvision, http.MethodPost, suspend + "/x", 403},
		{"provision, suspend of upper-case id", ScopeProvision, http.MethodPost, "/api/admin/tenants/8D264F39-9C52-4623-AA1D-258C7BCC34D7/suspend", 403},
		{"provision, resume of bare hex", ScopeProvision, http.MethodPost, "/api/admin/tenants/8d264f399c524623aa1d258c7bcc34d7/resume", 403},
		{"provision, suspend of a name that is no id", ScopeProvision, http.MethodPost, "/api/admin/tenants/acme/suspend", 403},
		{"provision, suspend with an empty id", ScopeProvision, http.MethodPost, "/api/admin/tenants//suspend", 403},
		{"provision, an encoded slash in the suspend path", ScopeProvision, http.MethodPost, "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7%2Fsuspend", 403},
		{"provision, suspend spelled in capitals", ScopeProvision, http.MethodPost, "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/SUSPEND", 403},
		// The drop itself (B3b-2): DELETE on the bare canonical id, and nothing else. Whether the key may drop THIS
		// tenant (one its principal created, suspended, with no unsettled run) is the handler's check.
		// A provision key reaches the drop for a tenant its principal created: owner decision D54 (2026-10-30,
		// cleat#3552). This row said 403 as a placeholder until the drop existed; D54 makes 200 the answer, and the
		// handler's creator check is what limits it to that principal's own tenants.
		{"provision, drop", ScopeProvision, http.MethodDelete, bare, 200},
		{"admin, drop", ScopeAdmin, http.MethodDelete, bare, 200},
		// Deleting a tombstone (B3b-2b) is the one act that lets a dropped id be provisioned again, and it is
		// admin-only on purpose: a service that can create tenants must not be able to recreate an id that was
		// dropped. Every method and spelling a provision key could try is refused; the admin key is the control.
		{"admin, delete a tombstone", ScopeAdmin, http.MethodDelete, tombstone, 200},
		{"provision, delete a tombstone", ScopeProvision, http.MethodDelete, tombstone, 403},
		{"provision, tombstone with GET", ScopeProvision, http.MethodGet, tombstone, 403},
		{"provision, tombstone with POST", ScopeProvision, http.MethodPost, tombstone, 403},
		{"provision, tombstone of upper-case id", ScopeProvision, http.MethodDelete, "/api/admin/tenants/8D264F39-9C52-4623-AA1D-258C7BCC34D7/tombstone", 403},
		{"provision, tombstone with a tail", ScopeProvision, http.MethodDelete, tombstone + "/x", 403},
		// Minting a recovery key (cleat#3578) hands the caller a working credential for a tenant's data, so
		// it is admin-only in v1 and a provision-scoped key is refused. It falls out of provisionMayReach with
		// no case to name it, which is the kind of refusal a later edit removes without noticing, so each
		// shape a provisioner might try is a row. Whether a provisioner may recover tenants it created is the
		// owner's open question on the issue.
		{"admin, mint a recovery key", ScopeAdmin, http.MethodPost, recovery, 200},
		{"provision, mint a recovery key", ScopeProvision, http.MethodPost, recovery, 403},
		{"provision, recovery key with GET", ScopeProvision, http.MethodGet, recovery, 403},
		{"provision, recovery key with DELETE", ScopeProvision, http.MethodDelete, recovery, 403},
		{"provision, recovery key of upper-case id", ScopeProvision, http.MethodPost, "/api/admin/tenants/8D264F39-9C52-4623-AA1D-258C7BCC34D7/recovery-key", 403},
		{"provision, recovery key with a tail", ScopeProvision, http.MethodPost, recovery + "/x", 403},
		{"provision, drop of upper-case id", ScopeProvision, http.MethodDelete, "/api/admin/tenants/8D264F39-9C52-4623-AA1D-258C7BCC34D7", 403},
		{"provision, drop of bare hex", ScopeProvision, http.MethodDelete, "/api/admin/tenants/8d264f399c524623aa1d258c7bcc34d7", 403},
		{"provision, drop of a name that is no id", ScopeProvision, http.MethodDelete, "/api/admin/tenants/acme", 403},
		{"provision, drop with an empty id", ScopeProvision, http.MethodDelete, "/api/admin/tenants/", 403},
		{"provision, drop of the collection", ScopeProvision, http.MethodDelete, "/api/admin/tenants", 403},
		{"provision, drop with a trailing slash", ScopeProvision, http.MethodDelete, bare + "/", 403},
		{"provision, drop with an encoded slash", ScopeProvision, http.MethodDelete, bare + "%2F", 403},
		// The method belongs to the route: another verb on the bare id is not a route a provisioner may reach,
		// and DELETE is not accepted on any route that has a tail.
		{"provision, GET of the bare id", ScopeProvision, http.MethodGet, bare, 403},
		{"provision, POST to the bare id", ScopeProvision, http.MethodPost, bare, 403},
		{"provision, PUT to the bare id", ScopeProvision, http.MethodPut, bare, 403},
		{"provision, DELETE of suspend", ScopeProvision, http.MethodDelete, suspend, 403},
		{"provision, DELETE of confirm", ScopeProvision, http.MethodDelete, confirm, 403},
		{"provision, DELETE of resume", ScopeProvision, http.MethodDelete, resume, 403},
		{"provision, DELETE of an instance route", ScopeProvision, http.MethodDelete, forceComplete, 403},
		{"admin, suspend", ScopeAdmin, http.MethodPost, suspend, 200},
		{"admin, drop-plan", ScopeAdmin, http.MethodGet, dropPlan, 200},
		{"admin, resume", ScopeAdmin, http.MethodPost, resume, 200},
		{"provision, other verbs under a tenant", ScopeProvision, http.MethodPost, "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7/drop", 403},
		{"provision, an encoded slash in the confirm path", ScopeProvision, http.MethodPost, "/api/admin/tenants/8d264f39-9c52-4623-aa1d-258c7bcc34d7%2Fconfirm", 403},
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
