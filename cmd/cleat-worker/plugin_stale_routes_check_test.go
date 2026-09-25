package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// staleRoutesPlugin implements the PRE-cleat#2232 RegisterRoutes signature
// (mux *http.ServeMux), which does not satisfy plugin.HasRoutes any more --
// exactly the fixture HasRoutes' own doc comment describes.
type staleRoutesPlugin struct{}

func (staleRoutesPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: "stale-routes"} }
func (staleRoutesPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }
func (staleRoutesPlugin) RegisterRoutes(mux *http.ServeMux) error                 { return nil }

// currentRoutesPlugin implements the CURRENT signature and must never be
// reported.
type currentRoutesPlugin struct{}

func (currentRoutesPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: "current-routes"} }
func (currentRoutesPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }
func (currentRoutesPlugin) RegisterRoutes(mux plugin.Router) error                  { return nil }

// noRoutesPlugin has no RegisterRoutes method at all -- a plugin with no HTTP
// routes, not a stale one, and must never be reported either.
type noRoutesPlugin struct{}

func (noRoutesPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: "no-routes"} }
func (noRoutesPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }

// TestCheckPluginRouteSignatures is cleat#2273 point 2 (detect a stale
// signature) plus cleat#2277 (refuse to start on one, rather than log and
// continue): HasRoutes' own doc comment says "a plugin whose signature does
// not match silently stops satisfying HasRoutes -- its routes would never
// register, and nothing would say why." Before #2273, nothing did; before
// #2277, main.go logged it and kept the worker running anyway.
func TestCheckPluginRouteSignatures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plug    plugin.Plugin
		wantErr bool
	}{
		{"pre-cleat#2232 signature refuses to start", staleRoutesPlugin{}, true},
		{"current signature does not refuse to start", currentRoutesPlugin{}, false},
		{"no RegisterRoutes method at all does not refuse to start", noRoutesPlugin{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugList := []*plugin.LoadedPlugin{{Plugin: tc.plug, Healthy: true}}

			err := checkPluginRouteSignatures(plugList)

			if (err != nil) != tc.wantErr {
				t.Fatalf("checkPluginRouteSignatures() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			if !strings.Contains(err.Error(), tc.plug.Info().Name) {
				t.Errorf("error does not name the offending plugin %q: %v", tc.plug.Info().Name, err)
			}
			if !strings.Contains(err.Error(), "RegisterRoutes") {
				t.Errorf("error does not mention RegisterRoutes, so a reader would not know what to fix: %v", err)
			}
		})
	}
}

// TestCheckPluginRouteSignaturesIgnoresNilPlugin guards the loop's own nil
// check -- plugList entries are always populated in production, but a
// defensive check that panics on nil is worse than one that is simply
// untested.
func TestCheckPluginRouteSignaturesIgnoresNilPlugin(t *testing.T) {
	plugList := []*plugin.LoadedPlugin{{Plugin: nil, Healthy: true}}

	if err := checkPluginRouteSignatures(plugList); err != nil {
		t.Errorf("expected no error for a nil plugin entry, got %v", err)
	}
}
