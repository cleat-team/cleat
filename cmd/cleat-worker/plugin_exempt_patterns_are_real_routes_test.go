package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"

	_ "github.com/cleat-team/cleat/plugins/oauthprovider"
	_ "github.com/cleat-team/cleat/plugins/slacknotify"
	_ "github.com/cleat-team/cleat/plugins/webhookingest"
)

// TestPluginAuthExemptPatternsAreRealRoutes is cleat-review's should-fix on
// #2318: pluginAuthExemptPatterns (plugin_exempt_routes.go) is matched
// against the real mux by exact pattern TEXT (auth.MiddlewareWithMux /
// auth.HostBindingMiddlewareWithMux, via isPublicRoute), not by any live
// relationship to what a plugin actually registers. If webhookingest's own
// mux.HandleFunc call in routes.go ever spells its wildcard differently --
// "{sid}" instead of "{source_id}", or the method changed -- every unit test
// on both sides could stay green (each side only ever compares itself to
// itself) while the real worker 401s every anonymous webhook, because the
// exempt list no longer names a pattern the mux resolves requests to at all.
// That is a FAIL-CLOSED break: unlike a wildcard silently exempting too much
// (cleat#2274, the bug this PR fixes), this direction breaks the deployment
// rather than a tenant's isolation, which is exactly why nothing else here
// would catch it -- every existing test either drives an in-memory mux built
// from the SAME literal list, or drives the plugin directly with no auth
// layer in front of it at all.
//
// This registers every bundled exempt-route plugin's REAL RegisterRoutes on
// a REAL *http.ServeMux -- the same construction main.go uses to build
// plugMux, skipping only Init (RegisterRoutes registers static patterns and
// does not read plugin state; see webhookingest/oauthprovider/slacknotify's
// RegisterRoutes, none of which touch p.* fields) -- then asks the mux which
// pattern a concrete request for each exempt entry resolves to, the same
// question isPublicRoute (public_route.go) asks in production.
func TestPluginAuthExemptPatternsAreRealRoutes(t *testing.T) {
	plugList, err := plugin.Discover()
	if err != nil {
		t.Fatalf("plugin.Discover: %v", err)
	}

	mux := http.NewServeMux()
	router := &pluginBodyLimitRouter{mux: mux, defaultLimit: 1 << 20}
	registered := 0
	for _, lp := range plugList {
		hr, ok := lp.Plugin.(plugin.HasRoutes)
		if !ok {
			continue
		}
		if err := hr.RegisterRoutes(router); err != nil {
			t.Fatalf("%s.RegisterRoutes: %v", lp.Plugin.Info().Name, err)
		}
		registered++
	}
	if registered == 0 {
		t.Fatal("PRECONDITION FAILED: no loaded plugin implements plugin.HasRoutes -- " +
			"the blank imports above did not register what this test expects, so the " +
			"assertions below would pass vacuously")
	}

	for _, pattern := range pluginAuthExemptPatterns {
		t.Run(pattern, func(t *testing.T) {
			method, path, ok := concretePathFor(pattern)
			if !ok {
				t.Fatalf("could not derive a concrete request from pattern %q -- update concretePathFor", pattern)
			}
			req := httptest.NewRequest(method, path, nil)
			_, resolved := mux.Handler(req)
			if resolved != pattern {
				t.Errorf("pluginAuthExemptPatterns has %q, but a %s %s request resolves to %q on the "+
					"real mux -- auth.MiddlewareWithMux/HostBindingMiddlewareWithMux compare against the "+
					"pattern TEXT, so this entry no longer exempts anything and every anonymous caller of "+
					"this route now gets 401 in production", pattern, method, path, resolved)
			}
		})
	}
}

// concretePathFor turns a Go 1.22+ http.ServeMux pattern ("METHOD /a/{b}/c")
// into one concrete request that pattern should match, by substituting a
// literal segment for each {wildcard}. Good enough for this test's three
// single-segment patterns; not a general pattern interpreter.
func concretePathFor(pattern string) (method, path string, ok bool) {
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	method = parts[0]
	segments := strings.Split(parts[1], "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segments[i] = "x"
		}
	}
	return method, strings.Join(segments, "/"), true
}
