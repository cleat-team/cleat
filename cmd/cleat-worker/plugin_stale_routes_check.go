package main

import (
	"fmt"
	"reflect"

	"github.com/cleat-team/cleat/plugin"
)

// registerRoutesMethodName is the loaded plugin's own doc comment on
// HasRoutes, RegisterRoutes(mux *http.ServeMux) error -- the signature every
// implementation in THIS tree carried before cleat#2232's breaking change to
// RegisterRoutes(mux Router) error. A plugin built against the old signature
// (an out-of-tree or vendored one that has not picked up the change) does
// not satisfy plugin.HasRoutes any more, and Go gives no error for that: the
// type assertion in the RegisterRoutes loop below just silently fails, and
// the plugin's routes never register.
const registerRoutesMethodName = "RegisterRoutes"

// checkPluginRouteSignatures refuses to start the worker if any loaded
// plugin's concrete type has a method literally named "RegisterRoutes" but
// does not satisfy plugin.HasRoutes -- the one situation HasRoutes' own doc
// comment warns about: "a plugin whose signature does not match silently
// stops satisfying HasRoutes -- its routes would never register, and nothing
// would say why."
//
// Before cleat#2277 this only logged an ERROR and continued, so a signature
// drift -- an out-of-tree or vendored plugin still built against the
// pre-#2232 RegisterRoutes(mux *http.ServeMux) error signature -- silently
// dropped that plugin's entire route table: requests to it fell through to
// the core catch-all's 404, and /readyz still answered 200, with nothing but
// a log line to show for it. Refusing to start makes a signature drift a
// deployment failure instead of a silent one, matching
// checkRequiredDeploymentSecrets' shape for the same reason: this is the
// only remaining point where the worker can still tell the difference.
//
// A plugin with NO method named RegisterRoutes at all is not an error --
// that is simply a plugin with no HTTP routes.
func checkPluginRouteSignatures(plugList []*plugin.LoadedPlugin) error {
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
		return fmt.Errorf("%s: has a RegisterRoutes method but does not satisfy plugin.HasRoutes, so its "+
			"routes would never register and nothing would ever say why -- update RegisterRoutes to take a "+
			"plugin.Router (not *http.ServeMux), the breaking signature change cleat#2232 made to every "+
			"in-tree plugin", lp.Plugin.Info().Name)
	}
	return nil
}
