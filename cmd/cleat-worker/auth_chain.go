package main

import (
	"net/http"

	"github.com/cleat-team/cleat/auth"
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
