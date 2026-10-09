package main

import "net/http"

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

// isPluginAuthExemptPattern reports whether pattern could serve any of the
// requests one of pluginAuthExemptPatterns is meant to -- not whether pattern
// is textually identical to one. cleat#2279 item 1: an exact string compare
// (the original form of this function) missed every equivalent spelling of
// the same route -- a differently-named wildcard ("POST /ingest/{sid}"), a
// method-less pattern that matches POST along with everything else
// ("/ingest/{source_id}"), a subtree pattern ("POST /ingest/{source_id}/"),
// or a host-qualified one ("POST example.com/ingest/{source_id}") -- so a
// plugin route spelled any of those ways was never flagged, and
// plugin.MaxBodyFromConfig's unconditional ceiling could be registered on a
// route this codebase intends to keep bounded by --plugin-max-body-size no
// matter what.
//
// pluginBodyLimitRouter uses this to refuse plugin.MaxBodyFromConfig's
// unconditional ceiling on exactly these patterns -- see
// plugin.MaxBodyFromConfig's doc comment for why an anonymously-reachable
// route must always stay bounded by the operator's --plugin-max-body-size
// flag, regardless of what a plugin's own config claims.
func isPluginAuthExemptPattern(pattern string) bool {
	for _, p := range pluginAuthExemptPatterns {
		if patternsOverlap(pattern, p) {
			return true
		}
	}
	return false
}

// patternsOverlap reports whether any single HTTP request could match both
// a and b, each read as an independent net/http.ServeMux pattern (Go 1.22+
// syntax: "[METHOD ][HOST]/PATH", where PATH segments can be a literal, a
// "{name}" wildcard, a "{name...}" trailing wildcard, or "{$}" to require an
// exact end).
//
// Deliberately conservative in one direction and not the other, because the
// two directions have different costs here. A pattern this reports as
// overlapping that is not truly auth-exempt in the live, fully-registered
// mux (for example a literal sibling of a wildcard, which net/http's real
// dispatch would route to the literal and auth.MiddlewareWithMux would then
// correctly treat as protected -- see auth/public_route.go's doc comment)
// only costs that route a forced plugin.MaxBody instead of
// plugin.MaxBodyFromConfig, which is always available as the fix. A pattern
// this reports as NOT overlapping when it actually is reachable with no
// credential is the thing cleat#2273 exists to prevent. So this asks the
// cheaper, over-inclusive question -- "could a and b, each considered ALONE,
// both match some request" -- rather than reproducing auth's sibling-aware,
// real-mux-dispatch precision, which needs every other route registered to
// answer correctly and isn't available at the point a single route is being
// checked in isolation.
//
// Implemented by delegating to two throwaway *http.ServeMuxes rather than
// hand-rolling the pattern grammar (wildcards, "{$}", trailing-slash subtree
// matching, host-qualification) a second time -- net/http already gets that
// right, including cases this file does not enumerate.
func patternsOverlap(a, b string) bool {
	if reqB, err := representativePatternRequest(b); err == nil {
		muxA := http.NewServeMux()
		muxA.HandleFunc(a, func(http.ResponseWriter, *http.Request) {})
		if _, matched := muxA.Handler(reqB); matched == a {
			return true
		}
	}
	if reqA, err := representativePatternRequest(a); err == nil {
		muxB := http.NewServeMux()
		muxB.HandleFunc(b, func(http.ResponseWriter, *http.Request) {})
		if _, matched := muxB.Handler(reqA); matched == b {
			return true
		}
	}
	return false
}

// representativePatternRequest builds one concrete *http.Request that
// pattern, registered alone on a *http.ServeMux, would match -- substituting
// a fixed literal for every wildcard segment, so the request is usable to
// probe a DIFFERENT mux for overlap. A pattern with no method matches every
// method; http.MethodGet is only this request's own representative verb,
// not a claim about what the pattern requires.
func representativePatternRequest(pattern string) (*http.Request, error) {
	method, host, path := splitMuxPattern(pattern)
	if method == "" {
		method = http.MethodGet
	}
	reqHost := host
	if reqHost == "" {
		reqHost = "plugin-exempt-route-overlap-probe.invalid"
	}
	req, err := http.NewRequest(method, "http://"+reqHost+instantiateMuxPath(path), nil)
	if err != nil {
		return nil, err
	}
	req.Host = reqHost
	return req, nil
}

// splitMuxPattern parses a net/http.ServeMux pattern into its method, host,
// and path components, per the grammar http2/net/http document: an optional
// "METHOD " prefix (a single space; method and host/path never contain
// spaces), then an optional host (everything before the first "/"), then the
// path.
func splitMuxPattern(pattern string) (method, host, path string) {
	rest := pattern
	if i := indexByte(rest, ' '); i >= 0 {
		method, rest = rest[:i], rest[i+1:]
	}
	if j := indexByte(rest, '/'); j >= 0 {
		host, path = rest[:j], rest[j:]
	} else {
		host, path = rest, "/"
	}
	return method, host, path
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// instantiateMuxPath replaces every wildcard segment in path with a fixed
// literal, so the result is a concrete path pattern's own representative
// request can use. "{$}" ends the path where it appears (it requires exactly
// that, with nothing further); "{name...}" consumes one literal segment and
// also ends the path there, since it is only ever the final segment in a
// valid pattern; "{name}" becomes one literal segment and parsing continues.
func instantiateMuxPath(path string) string {
	segments := splitByte(path, '/')
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch {
		case seg == "{$}":
			return joinByte(out, '/')
		case len(seg) > len("{...}")-1 && seg[0] == '{' && seg[len(seg)-4:] == "...}":
			out = append(out, "x")
			return joinByte(out, '/')
		case len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}':
			out = append(out, "x")
		default:
			out = append(out, seg)
		}
	}
	return joinByte(out, '/')
}

func splitByte(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func joinByte(segments []string, sep byte) string {
	out := make([]byte, 0, len(segments)*4)
	for i, seg := range segments {
		if i > 0 {
			out = append(out, sep)
		}
		out = append(out, seg...)
	}
	return string(out)
}
