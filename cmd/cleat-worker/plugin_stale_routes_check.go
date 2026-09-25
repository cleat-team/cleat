package main

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/cleat-team/cleat/plugin"
)

// staleRegisterRoutesMethodName is the loaded plugin's own doc comment on
// HasRoutes, RegisterRoutes(mux *http.ServeMux) error -- the signature every
// implementation in THIS tree carried before cleat#2232's breaking change to
// RegisterRoutes(mux Router) error. A plugin built against the old signature
// (an out-of-tree or vendored one that has not picked up the change) does
// not satisfy plugin.HasRoutes any more, and Go gives no error for that: the
// type assertion in the RegisterRoutes loop below just silently fails, and
// the plugin's routes never register.
const registerRoutesMethodName = "RegisterRoutes"

// warnAboutStalePluginRouteSignatures logs an ERROR for every loaded plugin
// whose concrete type has a method literally named "RegisterRoutes" but does
// not satisfy plugin.HasRoutes -- the one situation HasRoutes' own doc
// comment warns about and nothing previously checked for: "a plugin whose
// signature does not match silently stops satisfying HasRoutes -- its routes
// would never register, and nothing would say why." This is that "why".
//
// A plugin with NO method named RegisterRoutes at all is not warned about --
// that is simply a plugin with no HTTP routes, which is not an error.
func warnAboutStalePluginRouteSignatures(logger *slog.Logger, workerID string, plugList []*plugin.LoadedPlugin) {
	for _, lp := range plugList {
		if lp.Plugin == nil {
			continue
		}
		if _, ok := lp.Plugin.(plugin.HasRoutes); ok {
			continue // satisfies the current interface; nothing stale here
		}
		if _, hasMethod := reflect.TypeOf(lp.Plugin).MethodByName(registerRoutesMethodName); !hasMethod {
			continue // no RegisterRoutes method at all -- this plugin has no routes
		}
		logger.ErrorContext(context.Background(),
			"plugin has a RegisterRoutes method but does not satisfy plugin.HasRoutes, so its routes will "+
				"never register and no error will ever say why -- update RegisterRoutes to take a plugin.Router "+
				"(not *http.ServeMux), the breaking signature change cleat#2232 made to every in-tree plugin",
			"worker_id", workerID, "plugin", lp.Plugin.Info().Name)
	}
}
