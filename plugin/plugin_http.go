package plugin

import "net/http"

// ServeMux is *http.ServeMux on the host, or interface{} in TinyGo WASM builds
// where net/http is unavailable.
type ServeMux = *http.ServeMux

// HasRoutes: plugin exposes HTTP endpoints.
type HasRoutes interface {
	Plugin
	RegisterRoutes(mux *http.ServeMux) error
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
