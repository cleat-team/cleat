package main

// cleat#2172: /slack/interactive carries no cleat API key (it is a POST from
// Slack's own servers, not an authenticated cleat client) and no Host
// binding either, so with --require-auth on it would 401 before
// handleInteractiveCallback's own signature check ever ran -- the first
// problem #2172 reported. The fix is a hand-maintained exemption, the same
// shape ingest and the OAuth callback already use, and it needs BOTH
// middleware call sites: auth.Middleware (always installed with
// --require-auth) and auth.HostBindingMiddleware (installed only with
// --require-host-match, but when it is, every non-exempt path needs a
// matching tenant domain, which Slack's request never carries).
//
// cleat#2273 replaced the two hand-copied exempt-path literals at these call
// sites with one shared pluginAuthExemptPatterns var (plugin_exempt_routes.go),
// so this test now checks two things instead of one: that both middleware
// call sites still spread THAT variable (rather than a literal list some
// future edit could drift from it), and that the variable itself still
// contains "POST /slack/interactive". Either half failing reopens #2172's
// first problem -- a dropped call site silently, a dropped list entry
// silently in a different place.
//
// cleat#2274 changed both call sites from auth.Middleware/HostBindingMiddleware
// to the *WithMux variants, passing the real serving mux so a literal sibling
// of a public wildcard (POST /ingest/sources beside POST /ingest/{source_id})
// can't be wrongly matched by a throwaway matcher that never saw it. The
// regexes below were updated to match; they still assert the same two things.
//
// This is a source scan, not a live-server test, for the same reason
// TestMainSwitchesOnClassifyPluginInitError (a_plugin_init_error_severity_test.go)
// and a_deployment_secrets_wiring_test.go are: main() wires --require-auth
// and --require-host-match into a chain of closures nothing outside main()
// can reach, so the wiring itself has no call site a behavioural test could
// exercise without standing up a real worker process.

import (
	"os"
	"regexp"
	"testing"
)

func TestSlackInteractiveIsExemptFromBothAuthMiddlewares(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	body := stripGoComments(string(src))

	for _, tc := range []struct {
		name string
		call *regexp.Regexp
	}{
		{
			name: "auth.HostBindingMiddlewareWithMux",
			call: regexp.MustCompile(`auth\.HostBindingMiddlewareWithMux\(authResolver, mux, pluginAuthExemptPatterns\.\.\.\)\(handler\)`),
		},
		{
			name: "auth.MiddlewareWithMux",
			call: regexp.MustCompile(`handler = auth\.MiddlewareWithMux\(authResolver, true, mux, pluginAuthExemptPatterns\.\.\.\)\(handler\)`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.call.MatchString(body) {
				t.Fatalf("could not find %s(authResolver, ..., mux, pluginAuthExemptPatterns...)(handler) in main.go -- "+
					"either the wiring was restructured (update the pattern above) or this call site stopped "+
					"spreading the shared exempt-pattern list, which would let it drift from the other call site "+
					"exactly as the two hand-copied literals this replaced once could", tc.name)
			}
		})
	}

	if !regexpContainsSlackInteractive.MatchString(joinPatterns(pluginAuthExemptPatterns)) {
		t.Errorf("pluginAuthExemptPatterns no longer includes \"POST /slack/interactive\" -- "+
			"without it, --require-auth (or --require-host-match) 401s Slack's own request before "+
			"handleInteractiveCallback's signature check ever runs, reopening cleat#2172's first problem.\n"+
			"pluginAuthExemptPatterns: %v", pluginAuthExemptPatterns)
	}
}

func joinPatterns(patterns []string) string {
	out := ""
	for _, p := range patterns {
		out += `"` + p + `" `
	}
	return out
}

var regexpContainsSlackInteractive = regexp.MustCompile(`"POST /slack/interactive"`)
