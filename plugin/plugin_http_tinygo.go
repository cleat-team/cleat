//go:build tinygo

package plugin

// ServeMux is interface{} in TinyGo WASM builds where net/http is unavailable.
type ServeMux = interface{}

// HasRoutes: plugin exposes HTTP endpoints.
type HasRoutes interface {
	Plugin
	RegisterRoutes(mux interface{}) error
}

// HasMiddleware: plugin wraps the HTTP handler chain.
type HasMiddleware interface {
	Plugin
	Middleware(next interface{}) interface{}
}
