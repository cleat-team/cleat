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
