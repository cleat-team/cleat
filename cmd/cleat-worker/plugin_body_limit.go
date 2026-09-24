package main

import (
	"net/http"

	"github.com/cleat-team/cleat/plugin"
)

// pluginBodyLimitRouter is the host-side half of cleat#2232, design A: it
// implements plugin.Router over the SAME *http.ServeMux every plugin route
// already registers on (plugMux, which main.go also reuses as the core
// mux -- see the block comment above the RegisterRoutes loop), and wraps
// every handler a plugin registers with a request-body ceiling before that
// handler ever runs.
//
// A wrapping Router rather than a split mux or a ResponseWriter-rewriting
// middleware -- both were considered and rejected on this issue (see the
// design comments on cleat#2232) because they either bound CORE routes
// underneath their own larger ceilings (a split mux sharing plugMux) or lost
// r.PathValue population (composing via ServeMux.Handler(r) instead of its
// own ServeHTTP dispatch). Wrapping the handler at the moment it is
// registered changes nothing about how the pattern is matched or dispatched
// -- mux.Handle(pattern, wrapped) is exactly what a plugin would have called
// itself, just with one extra layer around the handler it passed in.
type pluginBodyLimitRouter struct {
	mux          *http.ServeMux
	defaultLimit int64
}

// Handle registers handler for pattern on the underlying mux, wrapped so its
// request body is capped before the plugin's own code runs. A route that
// declared its own ceiling via plugin.MaxBody uses that instead of
// defaultLimit -- checked here, once, at registration time, rather than on
// every request.
func (r *pluginBodyLimitRouter) Handle(pattern string, handler http.Handler) {
	limit := r.defaultLimit
	if declared, ok := plugin.MaxBodyLimit(handler); ok {
		limit = declared
	}
	r.mux.Handle(pattern, boundPluginRequestBody(limit, handler))
}

// HandleFunc is Handle for a plain handler func, matching plugin.Router's
// other method -- the large majority of plugin routes never call MaxBody and
// reach the host through this one instead.
func (r *pluginBodyLimitRouter) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	r.Handle(pattern, http.HandlerFunc(handler))
}

// boundPluginRequestBody wraps h so its request body is capped at limit
// bytes before h ever sees the request. This is the ONLY MaxBytesReader call
// in the plugin-route path -- TestEveryBoundedBodyGoesThroughTheHelper (see
// body_limit_413_names_the_limit_test.go) requires every MaxBytesReader call
// in this package to live inside a named allowed function, the same
// discipline cleat#1338 established for decodeBody/readBody on the core API.
//
// This does not itself translate *http.MaxBytesError into a 413 -- that is
// plugin.ReadBody/ReadJSONBody's job (plugin/body.go), running inside the
// plugin, after this wrap has already replaced r.Body. Splitting it this way
// keeps the ceiling and its enforcement point (here, at the host boundary,
// where every plugin route passes through regardless of which one it is) and
// the translation (there, once per plugin package, shared by every handler in
// it) each written exactly once.
func boundPluginRequestBody(limit int64, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		h.ServeHTTP(w, r)
	})
}
