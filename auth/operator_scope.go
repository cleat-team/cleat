package auth

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// An operator key can be NARROWED. cleat#3552 (B2b-1).
//
// WHY. Until this, every operator key reached everything under /api/admin/*:
// drain the worker, force-complete any tenant's instance, sweep retention. That is
// right for the person who runs the deployment and far too much for a SERVICE that
// only needs to create tenants. Handing that service an operator key would give it
// a credential good for every tenant in the deployment, which is the thing the
// 2026-09-15 tenant-relationships decision exists to prevent. So a key carries a
// scope, and the middleware admits it only to the routes its scope names.
//
// THE RULES, and the second is the one that matters:
//
//	admin      every /api/admin/* route. This is what every key meant before scopes
//	           existed, and it is the column default, so no existing key changes.
//	provision  only the routes in provisionMayReach. A service that creates tenants.
//
//	An operator whose scopes admit nothing reaches NOTHING. An empty list, an
//	unknown scope, a scope this binary does not know (written by a newer one) are
//	all "no routes", never "all routes". The gate is deny-by-default because the
//	failure it exists to prevent is a narrowed key that silently isn't.
const (
	ScopeAdmin     = "admin"
	ScopeProvision = "provision"
)

// operatorScopeRe is a scope name's shape. Names are fixed by this file, so this
// guards the stored text more than the vocabulary.
var operatorScopeRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// principalRe is what a principal name may look like: lower-case, a hyphen or dot
// or underscore inside, at most 128 characters. The same shape as a webhook name,
// for the same reason: it is typed by a person, compared byte for byte, and ends up
// in logs.
var principalRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// isKnownOperatorScope reports whether this binary defines the scope.
func isKnownOperatorScope(s string) bool {
	return s == ScopeAdmin || s == ScopeProvision
}

// ParseOperatorScopes reads the stored text. Unknown names are KEPT, not dropped:
// a key minted by a newer binary with a scope this one does not know must stay
// narrowed here, and dropping the unknown name would turn "provision + something
// new" into "provision" silently. MayReach is what treats them as granting nothing.
func ParseOperatorScopes(stored string) []string {
	return strings.Fields(stored)
}

// FormatOperatorScopes validates and canonicalises scopes for storage: at least
// one, each known, no duplicates, sorted so the same set always stores the same
// text. It refuses rather than defaults, because a mint that quietly widened an
// empty request to "admin" is the opposite of what a scope is for.
func FormatOperatorScopes(scopes []string) (string, error) {
	if len(scopes) == 0 {
		return "", fmt.Errorf("auth: an operator key needs at least one scope (%s or %s)", ScopeAdmin, ScopeProvision)
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if !operatorScopeRe.MatchString(s) || !isKnownOperatorScope(s) {
			return "", fmt.Errorf("auth: unknown operator scope %q (want %s or %s)", s, ScopeAdmin, ScopeProvision)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " "), nil
}

// ValidPrincipal reports whether s is an acceptable principal name.
func ValidPrincipal(s string) bool { return principalRe.MatchString(s) }

// IsAdmin reports whether the key holds the admin scope, which reaches every route and every tenant.
// A handler asks this when the answer is "any tenant" versus "only tenants this key's principal
// created"; which ROUTES a key may reach is MayReach's question, and this is not a way around it.
func (o Operator) IsAdmin() bool {
	for _, s := range ParseOperatorScopes(o.Scopes) {
		if s == ScopeAdmin {
			return true
		}
	}
	return false
}

// MayReach reports whether any of the operator's scopes admits the request.
// OperatorMiddleware calls it for every request an operator key makes, after the
// /api/admin/ confinement, so it decides among the routes that confinement leaves.
func (o Operator) MayReach(method string, u *url.URL) bool {
	for _, s := range ParseOperatorScopes(o.Scopes) {
		switch s {
		case ScopeAdmin:
			return true
		case ScopeProvision:
			if provisionMayReach(method, u) {
				return true
			}
		}
		// Anything else, including a scope this binary does not know, admits nothing.
	}
	return false
}

// provisionMayReach is the allow-list of the provision scope, and it is EXACT.
//
// Exact on purpose. "/api/admin/tenants/" with a trailing slash is the existing
// subtree for acting on a named tenant (instances, force-complete), so matching by
// prefix, or accepting the slash form, would hand a tenant-creating service every
// admin action on every tenant. The one route is the collection itself.
//
// CANONICAL FORM ONLY: a request whose path carries any percent-encoding the
// standard library would not itself have produced (url.URL.RawPath is non-empty) is
// refused. The check compares the DECODED path, and the mux routes on the ENCODED
// one: an encoded slash inside a segment decodes to the allowed path here while
// the mux treats it as one opaque segment and hands it to a different handler
// (cleat-review measured this against a real mux on cleat#3554: the SPA answered).
// The two disagreeing is the whole class of bug, so rather than reason about what
// the mux does with each spelling, a scoped key is held to the one spelling that
// cannot differ. A client has no reason to encode this path.
//
// Two routes: the create, and the confirm that ends the handshake (see auth/tenant_provision.go).
// Both were listed here before their handlers existed, because the gate has to exist before the
// thing it gates does: a handler added without it would be reachable by every operator key
// the moment it was registered.
func provisionMayReach(method string, u *url.URL) bool {
	if method != http.MethodPost || u.RawPath != "" {
		return false
	}
	if u.Path == OperatorRoutePrefix+"tenants" {
		return true
	}
	// The last step of the handshake, and a tenant's suspension, which a drop requires first
	// (cleat#3552, B3a): POST /api/admin/tenants/<id>/{confirm,suspend,resume}. The id must be in
	// the one canonical spelling (lower-case, hyphenated) so that the segment this gate saw is
	// byte for byte the one the mux and the handler see. The gate says only that the KEY may
	// reach the route; that it may act on THIS tenant (one its principal created) is the
	// handler's check, because it needs the tenant's row.
	rest, found := strings.CutPrefix(u.Path, OperatorRoutePrefix+"tenants/")
	if !found {
		return false
	}
	id, tail, hasTail := strings.Cut(rest, "/")
	if !hasTail || (tail != "confirm" && tail != "suspend" && tail != "resume") {
		return false
	}
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}
