package main

// pluginAuthExemptPatterns is the production list of plugin routes reachable
// with no cleat credential at all: an inbound webhook sender
// (plugins/webhookingest, verifies its own HMAC signature), a third-party
// IdP's OAuth login-start and redirect-back (plugins/oauthprovider,
// cleat#2319 added the former -- a browser starting a login has no cleat
// credential to present any more than the callback it leads to does), and
// Slack's own interactive-callback POST (plugins/slacknotify, cleat#2172).
// See auth.MiddlewareWithMux's doc comment for why this is a hand-maintained list
// rather than something plugins declare themselves.
//
// A single shared variable, not three hand-copied literals -- before
// cleat#2273's fix, this exact list was typed out separately at both
// auth.HostBindingMiddleware's and auth.MiddlewareWithMux's call sites in main.go,
// plus a fourth copy in plugin_route_body_limit_exempt_test.go. A pattern
// added to one and missed in another fails exactly the way cleat#2172's
// first problem did: silently, as a 401 on a request that has no way to
// carry a cleat API key. isPluginAuthExemptPattern (plugin_body_limit.go)
// is the other consumer -- it is what makes MaxBodyFromConfig's global-flag
// guarantee hold on exactly these patterns, rather than on whatever a
// plugin_body_limit.go maintainer remembers to list.
var pluginAuthExemptPatterns = []string{
	"POST /ingest/{source_id}",
	"GET /oauth/{provider}/login",
	"GET /oauth/{provider}/callback",
	"POST /slack/interactive",
}

// isPluginAuthExemptPattern reports whether pattern is one of the routes
// reachable with no cleat credential at all. pluginBodyLimitRouter uses this
// to refuse plugin.MaxBodyFromConfig's unconditional ceiling on exactly
// these patterns -- see plugin.MaxBodyFromConfig's doc comment for why an
// anonymously-reachable route must always stay bounded by the operator's
// --plugin-max-body-size flag, regardless of what a plugin's own config
// claims.
func isPluginAuthExemptPattern(pattern string) bool {
	for _, p := range pluginAuthExemptPatterns {
		if p == pattern {
			return true
		}
	}
	return false
}
