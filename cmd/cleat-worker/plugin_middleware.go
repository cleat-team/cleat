package main

import (
	"net/http"

	"github.com/cleat-team/cleat/plugin"
)

// wrapPluginMiddleware wraps base in every healthy plugin's Middleware, in plugin order, so the
// LAST plugin is the outermost. It is a function, and not a loop inside main(), so a test can serve a
// real route through the same chain the worker serves: every plugin middleware wraps the CORE mux
// (cleat#1569), which means each http.ResponseWriter wrapper a plugin installs sits in front of
// every core handler, streaming ones included. cleat#2254: audit-log's wrapper had no Flush, and on
// every default build GET /api/workflows/:id/stream answered 500 "streaming not supported".
func wrapPluginMiddleware(base http.Handler, plugList []*plugin.LoadedPlugin) http.Handler {
	h := base
	for _, lp := range plugList {
		if !lp.Healthy {
			continue
		}
		if p, ok := lp.Plugin.(plugin.HasMiddleware); ok {
			h = p.Middleware(h)
		}
	}
	return h
}
