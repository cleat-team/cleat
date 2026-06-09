//go:build tinygo

package plugin

// ServeMux is interface{} in TinyGo WASM builds where net/http is unavailable.
type ServeMux = any

// HasRoutes: plugin exposes HTTP endpoints.
type HasRoutes interface {
	Plugin
	RegisterRoutes(mux any) error
}

// HasMiddleware: plugin wraps the HTTP handler chain.
type HasMiddleware interface {
	Plugin
	Middleware(next any) any
}
