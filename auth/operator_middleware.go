package auth

import (
	"net/http"
	"strings"
)

// OperatorRoutePrefix is the surface an operator credential may reach.
//
// cleat#2169 asks for two things from the credential and this constant is the
// second of them: a request carrying an operator key "may target any tenant on
// admin routes, and never reach tenant-scoped data paths as a tenant". The
// first half is not implemented yet -- acting on another tenant's workflow
// needs a cross-tenant lookup this tree does not have, and callerOwnsTarget
// still answers 401 for a request with no tenant (see its own comment). The
// second half is enforced here, and it is the half that must exist BEFORE the
// first: an operator key that reached /api/workflows would arrive with no
// tenant in its context, and the tenancy checks that read "no tenant" as
// "authentication is disabled" are exactly the code that must never see it.
//
// /api/admin/* is itself gated on --enable-admin-api (apiServer.adminAPIOnly),
// which answers 404 while that flag is off. So an operator key can reach only
// what the deployment has already chosen to expose, and turning the admin API
// off closes it to operators too rather than leaving a second door.
const OperatorRoutePrefix = "/api/admin/"

// OperatorMiddleware authenticates a request that carries an operator key and
// confines it to OperatorRoutePrefix.
//
// It goes OUTSIDE MiddlewareWithMux in the chain (cmd/cleat-worker/main.go),
// and has to: that middleware resolves a key against admin.tenant_api_keys and
// answers 401 "invalid or revoked API key" to everything else, so an operator
// key would be refused as a bad tenant key before anything could recognise it.
// Being outside also means an operator request never reaches
// HostBindingMiddlewareWithMux, which is correct rather than incidental -- host
// binding exists to stop one tenant's key being used against another tenant's
// domain, and an operator has no domain to compare.
//
// Every request that does NOT carry an operator-shaped key is passed through
// untouched, so this is additive: with no operator keys in the table the chain
// behaves exactly as it did before, and a request with an ordinary tenant key,
// no key at all, or an unknown bearer token reaches the same code it always
// did.
func OperatorMiddleware(store OperatorResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := extractAPIKey(r)
			if key == "" || !IsOperatorKey(key) {
				// Not an operator credential -- including the common case of
				// no credential at all. Pass it inward unchanged.
				next.ServeHTTP(w, r)
				return
			}

			// Resolve BEFORE confining, so the two answers mean what they say:
			// a caller presenting a bad credential is unauthenticated (401),
			// and one presenting a good credential on the wrong route is
			// authenticated but not permitted (403). Confining first would
			// answer 403 to a forged key, which reports the wrong one of those
			// two facts -- and it is the fact an operator debugging a
			// deployment reads first.
			op, err := OperatorFromAPIKey(r.Context(), store, sha256Hash(key))
			if err != nil {
				// Any error, including the database being unreachable. That
				// conflates "no such key" with "I could not check", which is
				// the same conflation auth.MiddlewareWithMux already makes for
				// tenant keys. It is mirrored deliberately rather than fixed
				// here: the remedy belongs in one place for both credentials,
				// and a fix applied to this one alone would give two answers
				// to the same question depending on which key was presented.
				http.Error(w, `{"error":"invalid or revoked operator key"}`, http.StatusUnauthorized)
				return
			}

			// The confinement, and it applies to the API only. A path outside
			// /api/ is left to the chain below exactly as a keyless request
			// would be: /livez and /metrics are answered without a credential
			// at all, and the SPA and plugin routes have their own rules.
			// Refusing an operator key there would make presenting one worse
			// than presenting nothing, which is a trap rather than a policy.
			if path := r.URL.Path; strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, OperatorRoutePrefix) {
				http.Error(w, `{"error":"an operator key may only be used on /api/admin/* routes"}`, http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithOperator(r.Context(), op)))
		})
	}
}
