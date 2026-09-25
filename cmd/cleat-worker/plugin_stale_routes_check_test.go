package main

import (
	"bytes"
	"context"
	"log/slog"
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
// warned about.
type currentRoutesPlugin struct{}

func (currentRoutesPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: "current-routes"} }
func (currentRoutesPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }
func (currentRoutesPlugin) RegisterRoutes(mux plugin.Router) error                  { return nil }

// noRoutesPlugin has no RegisterRoutes method at all -- a plugin with no HTTP
// routes, not a stale one, and must never be warned about either.
type noRoutesPlugin struct{}

func (noRoutesPlugin) Info() plugin.PluginInfo                                 { return plugin.PluginInfo{Name: "no-routes"} }
func (noRoutesPlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }

// TestWarnAboutStalePluginRouteSignatures is cleat#2273 point 2: HasRoutes'
// own doc comment says "a plugin whose signature does not match silently
// stops satisfying HasRoutes -- its routes would never register, and nothing
// would say why." Before this test, nothing did.
func TestWarnAboutStalePluginRouteSignatures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plug     plugin.Plugin
		wantWarn bool
	}{
		{"pre-cleat#2232 signature is warned about", staleRoutesPlugin{}, true},
		{"current signature is not warned about", currentRoutesPlugin{}, false},
		{"no RegisterRoutes method at all is not warned about", noRoutesPlugin{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			plugList := []*plugin.LoadedPlugin{{Plugin: tc.plug, Healthy: true}}

			warnAboutStalePluginRouteSignatures(logger, "w1", plugList)

			got := strings.Contains(buf.String(), tc.plug.Info().Name)
			if got != tc.wantWarn {
				t.Errorf("warned=%v, want %v.\nlog output: %s", got, tc.wantWarn, buf.String())
			}
			if tc.wantWarn && !strings.Contains(buf.String(), "RegisterRoutes") {
				t.Errorf("warning does not mention RegisterRoutes, so a reader would not know what to fix: %s", buf.String())
			}
		})
	}
}

// TestWarnAboutStalePluginRouteSignaturesIgnoresNilPlugin guards the loop's
// own nil check -- plugList entries are always populated in production, but
// a defensive check that panics on nil is worse than one that is simply
// untested.
func TestWarnAboutStalePluginRouteSignaturesIgnoresNilPlugin(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	plugList := []*plugin.LoadedPlugin{{Plugin: nil, Healthy: true}}

	warnAboutStalePluginRouteSignatures(logger, "w1", plugList)
}
