package plugin

import "net/http"

// ServeMux is *http.ServeMux on the host, or interface{} in TinyGo WASM builds
// where net/http is unavailable.
type ServeMux = *http.ServeMux

// Router is the minimal registration surface RegisterRoutes needs.
// *http.ServeMux satisfies it structurally, so the host can pass either the
// real mux or an adapter over it -- cleat#2232's own host adapter wraps every
// handler with a request-body ceiling (--plugin-max-body-size, or a larger
// one a route declares via MaxBody) before it reaches the plugin, entirely
// outside this interface. RegisterRoutes' body does not change: it already
// only calls mux.HandleFunc/mux.Handle, both of which Router declares with
// the identical signature net/http.ServeMux uses.
//
// A NARROWER interface than *http.ServeMux, not a wider one -- Handler and
// ServeHTTP are deliberately left off. Neither is something RegisterRoutes
// calls, and including them would let a future adapter be handed a mux it
// could read routing decisions FROM as well as register TO, which is not a
// capability any plugin needs.
type Router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// HasRoutes: plugin exposes HTTP endpoints.
//
// RegisterRoutes took *http.ServeMux until cleat#2232, which found 26 sites
// across 17 plugins reading a request body with no ceiling at all -- reachable
// anonymously through /ingest/{source_id} and /slack/interactive, which are
// exempt from tenant auth by design. A plugin cannot be handed a size limit
// through a mux it registers ON, only through one it registers WITH, so the
// parameter became an interface the host's own adapter can sit behind. This
// is a breaking plugin-API change (owner decision, cleat#2232): every
// RegisterRoutes implementation in this tree took the one-line signature
// update (mux *http.ServeMux -> mux Router) in the same change, since a
// plugin whose signature does not match silently stops satisfying HasRoutes
// -- its routes would never register, and nothing would say why.
type HasRoutes interface {
	Plugin
	RegisterRoutes(mux Router) error
}

// HasMiddleware: plugin wraps the HTTP handler chain.
type HasMiddleware interface {
	Plugin
	Middleware(next http.Handler) http.Handler
}

// EgressTransport is the http.RoundTripper a plugin must use for every outbound
// request. cleat#1565.
//
// An alias rather than an import in plugin.go, matching ServeMux above: that
// file deliberately names standard-library types without importing them, so a
// TinyGo guest build is not dragged through net/http.
//
// WHY A TRANSPORT AND NOT A CLIENT. Plugins build their client once, at Init,
// with their own timeout -- 10s for Slack, longer for an LLM. A shared client
// would flatten those. A transport preserves each plugin's timeout and still
// routes every dial through the egress policy.
//
// WHY IT WORKS DESPITE BEING BUILT ONCE. The policy is per tenant and the
// tenant is per CALL, which looks like a contradiction. It is not: the guard's
// decision runs inside DialContext, which receives the context of the REQUEST
// being dialled. Every one of these plugins already uses
// http.NewRequestWithContext, so the tenant travels with the request and a
// single transport resolves it per call.
type EgressTransport = http.RoundTripper
