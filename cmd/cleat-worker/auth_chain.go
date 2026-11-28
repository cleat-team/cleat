package main

import (
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/auth"
	"github.com/google/uuid"
)

// authChain is what composeAuthChain needs to wrap the API in authentication.
type authChain struct {
	// Tenants resolves a tenant API key. Always required.
	Tenants auth.TenantResolver
	// Operators resolves an operator key. Always required.
	Operators auth.OperatorResolver
	// Domains is the host-binding resolver, consulted only under RequireHostMatch.
	Domains          auth.DomainResolver
	RequireHostMatch bool
}

// composeAuthChain wraps inner in the authentication layers, in the one order that is correct, and
// is the ONLY place that order is written down. main.go calls it, and so does the test that
// measures it (a_minted_operator_key_reaches_an_admin_route_on_a_real_worker_test.go).
//
// It was inline in main.go until cleat#3555, and that is how the bug it fixes survived: every unit
// test of the operator layer ran it over a stub, or installed the operator in the request context
// by hand, so no test composed it with the tenant layer it sits in front of. A chain that can only
// be built inside a 3000-line main() cannot be tested as built.
//
// Outermost to innermost:
//
//	OperatorMiddleware        resolves cleat_op_ keys; an admitted request is served by inner
//	  MiddlewareWithMux       resolves tenant keys; 401s anything it cannot
//	    HostBindingMiddleware (only under RequireHostMatch) a tenant's key against its domain
//	      inner
//
// HOST BINDING IS WRAPPED FIRST so that after the tenant layer wraps it the order is auth OUTSIDE,
// host binding INSIDE. Installed the other way round, the check would run before any credential had
// been resolved and would read whatever the tenant-resolver middleware had put in the context --
// which on some paths is a value the CLIENT supplied. cleat#1568.
//
// The operator layer is outside both, and hands an admitted operator request to inner directly:
// around the tenant layer, which would otherwise re-resolve the operator key as a tenant key and
// answer 401 (cleat#3555), and around host binding, which an operator has no domain to satisfy.
func composeAuthChain(inner http.Handler, mux *http.ServeMux, c authChain) http.Handler {
	handler := inner
	if c.RequireHostMatch {
		handler = auth.HostBindingMiddlewareWithMux(c.Domains, mux, pluginAuthExemptPatterns...)(handler)
	}
	handler = auth.MiddlewareWithMux(c.Tenants, true, mux, pluginAuthExemptPatterns...)(handler)
	return auth.OperatorMiddleware(c.Operators, inner)(handler)
}

// composeRequestChain is the whole of what main.go puts around the API's handler for identity: the
// authentication layers when there are any, then the --tenant-resolver wrapper OUTSIDE them. main.go
// calls it, and so do the tests that measure the order (cleat#3577).
//
// THE HEADER WRAPPER MUST BE THE OUTERMOST LAYER, and that is the correctness argument, not
// a tidy-up. Outside, it runs first: it puts the header's tenant in the context, and the tenant-key
// middleware inside it then sets the tenant from the KEY and shadows the header. Installed inside
// the chain it would run last and shadow the key instead, so a request carrying a valid key for
// tenant A and `X-Tenant: <B>` would act on tenant B -- for /api/keys that is minting, listing and
// revoking another tenant's keys. It held for months only by the order of two blocks in main(), and
// nothing could see it: the header wrapper was not part of composeAuthChain.
//
// chain is nil when authentication is off (--require-auth=false); then the header is the only
// identity there is, which is why /api/keys answers 403 in that mode.
func composeRequestChain(inner http.Handler, mux *http.ServeMux, chain *authChain, tenantResolver string) http.Handler {
	handler := inner
	if chain != nil {
		handler = composeAuthChain(handler, mux, *chain)
	}
	return tenantResolverMiddleware(tenantResolver)(handler)
}

// tenantResolverMiddleware is --tenant-resolver=header:<name>: a valid uuid in that header becomes
// the request's tenant. Any other mode, and a header that is absent or not a uuid, leave the request
// as it came. It is only ever correct as the outermost layer; see composeRequestChain.
func tenantResolverMiddleware(mode string) func(http.Handler) http.Handler {
	headerName, ok := strings.CutPrefix(mode, "header:")
	if !ok {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tidStr := r.Header.Get(headerName); tidStr != "" {
				if tid, err := uuid.Parse(tidStr); err == nil {
					r = r.WithContext(auth.WithTenantID(r.Context(), tid))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
