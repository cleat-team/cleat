package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// TestPluginRouteBodyLimitAppliesOnBothAuthExemptRoutes is cleat#2232 design
// item 7.
//
// "POST /ingest/{source_id}" and "POST /slack/interactive" are the two plugin
// routes reachable with no cleat credential at all -- both are listed in
// auth.Middleware's and auth.HostBindingMiddleware's publicPatterns in
// main.go, by design, because an inbound webhook or a Slack callback cannot
// present a cleat API key. That is exactly the shape #2232 was filed
// against: a request needing no credential can reach a plugin handler with
// an unbounded body, unless something bounds it ahead of the exemption
// rather than as part of the auth check the exemption skips.
//
// main() itself is not reachable from a test (see
// a_slack_interactive_route_is_exempt_test.go's doc comment: --require-auth
// and --require-host-match are wired into a closure chain nothing outside
// main() can reach), so this builds the same three-layer composition
// independently -- the real auth.Middleware and auth.HostBindingMiddleware
// constructors, the production exempt-path list copied verbatim from
// main.go, wrapped around the real pluginBodyLimitRouter
// (plugin_body_limit.go) -- rather than trusting that main()'s comments
// describe what its wiring actually does.
//
// store and resolver are both nil: an exempt path never reaches
// auth.TenantFromAPIKey or auth.DomainResolver.TenantForHost, so a nil
// TenantResolver/DomainResolver is never dereferenced. If either exemption
// stopped matching, this test would panic on a nil call rather than merely
// failing an assertion -- itself a signal, not a flaw in the test.
func TestPluginRouteBodyLimitAppliesOnBothAuthExemptRoutes(t *testing.T) {
	const limit = 64

	for _, route := range []struct {
		name    string
		pattern string
		path    string
	}{
		{"ingest", "POST /ingest/{source_id}", "/ingest/src-1"},
		{"slack_interactive", "POST /slack/interactive", "/slack/interactive"},
	} {
		t.Run(route.name, func(t *testing.T) {
			var bodyProcessed bool
			plugMux := http.NewServeMux()
			router := &pluginBodyLimitRouter{mux: plugMux, defaultLimit: limit}
			router.HandleFunc(route.pattern, func(w http.ResponseWriter, r *http.Request) {
				// The real per-plugin division of labour: the host wrap
				// (boundPluginRequestBody) only sets the ceiling on r.Body --
				// it writes no response itself. plugin.ReadBody is what
				// turns an *http.MaxBytesError into 413, exactly as every
				// converted plugin route now does (cleat#2232 item 2).
				body, ok := plugin.ReadBody(w, r)
				if !ok {
					return
				}
				bodyProcessed = true
				_ = body
				w.WriteHeader(http.StatusOK)
			})

			// Same two constructors, the SAME shared exempt list main.go
			// spreads at both its own call sites (pluginAuthExemptPatterns,
			// plugin_exempt_routes.go -- cleat#2273 replaced the hand-copied
			// literal this test used to carry, so a drift between this test
			// and main.go is no longer possible by construction), same
			// nesting order as main.go (auth.Middleware wraps
			// HostBindingMiddleware wraps the mux).
			var handler http.Handler = plugMux
			handler = auth.HostBindingMiddleware(nil, pluginAuthExemptPatterns...)(handler)
			handler = auth.Middleware(nil, true, pluginAuthExemptPatterns...)(handler)

			// THE CONTROL COMES FIRST. Without it, "the oversized one got
			// 413" would pass just as well against a chain that 401s or
			// 413s everything -- including a chain where the exempt-path
			// list above has silently drifted from main.go's and every
			// anonymous request is being rejected by auth, not by the body
			// ceiling this test is about.
			bodyProcessed = false
			req := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader("ok"))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("control: anonymous %s got %d, want 200 (body: %s) -- either the "+
					"exempt-path list here no longer matches main.go's, or auth.Middleware/"+
					"HostBindingMiddleware rejected an anonymous request this route must accept",
					route.path, w.Code, w.Body.String())
			}
			if !bodyProcessed {
				t.Fatalf("control: a %d-byte body was not accepted by plugin.ReadBody", len("ok"))
			}

			bodyProcessed = false
			oversized := strings.Repeat("x", limit+1024)
			req = httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(oversized))
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized, anonymous %s got %d, want 413 (body: %s) -- an "+
					"unauthenticated caller on this route can send an unbounded body",
					route.path, w.Code, w.Body.String())
			}
			if bodyProcessed {
				t.Fatalf("plugin.ReadBody reported success for a body over the %d-byte limit", limit)
			}
			if want := "64 bytes"; !strings.Contains(w.Body.String(), want) {
				t.Errorf("413 body %q does not name the limit it hit (%s)", w.Body.String(), want)
			}
		})
	}
}

// TestMaxBodyFromConfigIsClampedOnAnAuthExemptRoute is cleat#2273's guard
// rail 1, end to end: if a plugin registered plugin.MaxBodyFromConfig on one
// of pluginAuthExemptPatterns -- reachable with no cleat credential at all --
// the host must refuse the plugin's own unconditional ceiling and fall back
// to defaultLimit, so the operator's --plugin-max-body-size flag still bounds
// every anonymous request regardless of what a plugin's config claims.
//
// This does not exercise a real plugin doing this (none does; blobstore,
// the only MaxBodyFromConfig user, registers on a tenant-scoped route). It
// proves the host-side safety net exists independently of any plugin
// currently behaving -- see plugin_body_limit_test.go's
// TestMaxBodyFromConfigOnAnExemptPatternFallsBackToTheDefault for the
// router-level unit test this corroborates at the real middleware chain.
func TestMaxBodyFromConfigIsClampedOnAnAuthExemptRoute(t *testing.T) {
	const defaultLimit = 64
	const hugeConfiguredLimit = 10 * 1024 * 1024

	var bodyProcessed bool
	plugMux := http.NewServeMux()
	router := &pluginBodyLimitRouter{mux: plugMux, defaultLimit: defaultLimit}
	router.Handle("POST /ingest/{source_id}", plugin.MaxBodyFromConfig(hugeConfiguredLimit, "some_plugin_setting", func(w http.ResponseWriter, r *http.Request) {
		body, ok := plugin.ReadBody(w, r)
		if !ok {
			return
		}
		bodyProcessed = true
		_ = body
		w.WriteHeader(http.StatusOK)
	}))

	var handler http.Handler = plugMux
	handler = auth.HostBindingMiddleware(nil, pluginAuthExemptPatterns...)(handler)
	handler = auth.Middleware(nil, true, pluginAuthExemptPatterns...)(handler)

	oversized := strings.Repeat("x", defaultLimit+1024)
	req := httptest.NewRequest(http.MethodPost, "/ingest/src-1", strings.NewReader(oversized))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413 -- a %d-byte body should have tripped the %d-byte default ceiling, "+
			"not the plugin's own %d-byte MaxBodyFromConfig declaration (body: %s)",
			w.Code, defaultLimit+1024, defaultLimit, hugeConfiguredLimit, w.Body.String())
	}
	if bodyProcessed {
		t.Fatal("plugin.ReadBody reported success -- the exempt-pattern safety clamp did not apply")
	}
	if want := fmt.Sprintf("%d bytes", defaultLimit); !strings.Contains(w.Body.String(), want) {
		t.Errorf("413 body %q does not name the clamped default limit (%s)", w.Body.String(), want)
	}
	if strings.Contains(w.Body.String(), "some_plugin_setting") {
		t.Errorf("413 body %q names the plugin's own knob even though it was refused on an exempt route",
			w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "--plugin-max-body-size") {
		t.Errorf("413 body %q does not fall back to naming --plugin-max-body-size", w.Body.String())
	}
}
