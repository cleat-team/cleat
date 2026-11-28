package main

// cleat#3632. plugins/oauthprovider/middleware.go said its middleware "sits OUTSIDE auth.MiddlewareWithMux".
// In cmd/cleat-worker it is the other way round: main builds plugHandler = wrapPluginMiddleware(mux, ...), and
// composeAuthChain wraps that, so a plugin's middleware runs AFTER core auth. The comment used that premise to
// argue that falling through for a token not shaped like a session is not a loosening.
//
// This pins the order the comment now states, through the two functions main uses, so a comment in another
// package cannot drift from the worker a second time. A plugin cannot import cmd/cleat-worker, which is why
// the measurement lives here and not next to the comment.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// recordingMiddleware is a plugin whose only job is to say whether it ran and what the context held when it did.
type recordingMiddleware struct {
	reached   *int
	hadTenant *bool
}

func (recordingMiddleware) Info() plugin.PluginInfo                         { return plugin.PluginInfo{Name: "recording"} }
func (recordingMiddleware) Init(context.Context, *plugin.Environment) error { return nil }
func (p recordingMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*p.reached++
		_, *p.hadTenant = auth.TenantIDFromContext(r.Context())
		next.ServeHTTP(w, r)
	})
}

func TestAPluginsMiddlewareRunsInsideCoreAuthThroughTheFunctionsMainUses(t *testing.T) {
	f := buildChain(t, false)
	var reached int
	var hadTenant bool
	mux := http.NewServeMux()
	registerRoutes(mux, f.api)
	keys := &keyTable{
		tenants:   map[[32]byte]uuid.UUID{sha256.Sum256([]byte(f.tenantAKey)): uuid.MustParse(tenantA)},
		operators: map[[32]byte]auth.Operator{},
	}
	chainCfg := authChain{Tenants: keys, Operators: keys, Domains: noDomains{asked: new(bool)}}
	plugins := []*plugin.LoadedPlugin{{Plugin: recordingMiddleware{&reached, &hadTenant}, Healthy: true}}

	// What main does, through the two functions it uses: the plugin middleware wraps the core mux (main.go builds
	// plugHandler = wrapPluginMiddleware(plugMux, plugList)), then core auth wraps that (handler := plugHandler,
	// then the chain). That main passes plugHandler into the chain is read from main.go, not measured here.
	inside := composeAuthChain(wrapPluginMiddleware(mux, plugins), mux, chainCfg)
	// The order the old comment described, built by hand: the plugin middleware OUTSIDE core auth.
	outside := wrapPluginMiddleware(composeAuthChain(mux, mux, chainCfg), plugins)

	hex64 := strings.Repeat("ab", 32) // the shape of a session token: 64 lowercase hex characters
	serve := func(h http.Handler, path, bearer string) int {
		reached, hadTenant = 0, false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	// KNOWN-POSITIVE: the harness can tell the two orders apart. With the plugin outside, a request core auth
	// will refuse still reaches the plugin, and does so before any tenant is known.
	if code := serve(outside, "/api/workflows", hex64); reached != 1 || hadTenant || code != http.StatusUnauthorized {
		t.Fatalf("the old comment's order (plugin outside core auth), 64-hex bearer = %d, plugin reached %d times, tenant in ctx %v; want 401, reached once, no tenant. The harness cannot see the order, so nothing below is evidence",
			code, reached, hadTenant)
	}

	// A valid key: the plugin middleware runs, and core auth has ALREADY put the tenant in the context.
	if code := serve(inside, "/api/workflows", f.tenantAKey); code != http.StatusOK || reached != 1 || !hadTenant {
		t.Errorf("valid key = %d, plugin reached %d times, tenant already in ctx %v; want 200, once, true. Plugin middleware must run inside core auth",
			code, reached, hadTenant)
	}

	// Nothing the plugin could add is reached by a request core auth refuses.
	for _, tc := range []struct{ name, path, bearer string }{
		{"no credential", "/api/workflows", ""},
		{"an unknown API key", "/api/workflows", "cleat_sk_unknown"},
		{"a 64-hex bearer, session-shaped, on a data route", "/api/workflows", hex64},
		{"a 64-hex bearer, session-shaped, on /api/keys", "/api/keys", hex64},
	} {
		if code := serve(inside, tc.path, tc.bearer); code != http.StatusUnauthorized || reached != 0 {
			t.Errorf("%s = %d, plugin reached %d times; want 401 and never. Core auth must refuse it before any plugin middleware runs", tc.name, code, reached)
		}
	}
}
