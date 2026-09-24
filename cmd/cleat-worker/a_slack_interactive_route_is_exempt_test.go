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
// This is a source scan, not a live-server test, for the same reason
// TestMainSwitchesOnClassifyPluginInitError (a_plugin_init_error_severity_test.go)
// and a_deployment_secrets_wiring_test.go are: main() wires --require-auth
// and --require-host-match into a chain of closures nothing outside main()
// can reach, so the wiring itself has no call site a behavioural test could
// exercise without standing up a real worker process. What this proves is
// narrower and cheaper: that the string a reviewer can point at with
// confidence -- "POST /slack/interactive" -- is still an argument to BOTH
// middleware constructors, not merely present somewhere in the file (a
// plain substring count of 2 would not tell you which call site lost it if
// one did).

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
		name  string
		block *regexp.Regexp
	}{
		{
			name:  "auth.HostBindingMiddleware",
			block: regexp.MustCompile(`(?s)auth\.HostBindingMiddleware\(authResolver,(.*?)\)\(handler\)`),
		},
		{
			name:  "auth.Middleware",
			block: regexp.MustCompile(`(?s)handler = auth\.Middleware\(authResolver, true,(.*?)\)\(handler\)`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.block.FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("could not find the %s(...) call in main.go -- the exempt-path wiring this test "+
					"checks may have been restructured; update the pattern above rather than deleting this test", tc.name)
			}
			if !regexpContainsSlackInteractive.MatchString(m[1]) {
				t.Errorf("%s's exempt-path list no longer includes \"POST /slack/interactive\" -- "+
					"without it, --require-auth (or --require-host-match) 401s Slack's own request before "+
					"handleInteractiveCallback's signature check ever runs, reopening cleat#2172's first problem.\n"+
					"block contents: %s", tc.name, m[1])
			}
		})
	}
}

var regexpContainsSlackInteractive = regexp.MustCompile(`"POST /slack/interactive"`)
