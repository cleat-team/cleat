package auth

import "net/http"

// publicPatternSet turns a hand-maintained pattern list into a lookup set.
// Returns nil for an empty list so callers can skip the check entirely.
func publicPatternSet(patterns []string) map[string]bool {
	if len(patterns) == 0 {
		return nil
	}
	set := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		set[p] = true
	}
	return set
}

// isPublicRoute decides whether r matches one of the exempt patterns.
//
// When mux is non-nil, it is the SAME *http.ServeMux that will actually serve
// the request (registered with every core and plugin route), and the
// decision is made by asking IT which pattern the request resolves to. That
// is deliberate: net/http's mux prefers the more specific of two overlapping
// patterns, so a literal sibling of a public wildcard -- "POST /ingest/sources"
// beside the public "POST /ingest/{source_id}" -- wins there exactly as it
// will when the request is really served, and the sibling is correctly
// reported as NOT public.
//
// A throwaway mux built from only the exempt patterns (publicMatcher) cannot
// do this: with no sibling registered on it, the wildcard is the only
// candidate and matches everything the sibling should have shadowed. That
// was cleat#2274 -- POST /ingest/sources, a real non-public route, matched
// the public "POST /ingest/{source_id}" and skipped API key resolution
// entirely.
//
// mux is nil for callers with no real serving mux to hand (chiefly tests that
// only exercise the exempt pattern itself, never a shadowed sibling); those
// fall back to publicMatcher, which is the pre-#2274 behavior and correct as
// long as nothing that could shadow an exempt pattern is in play.
func isPublicRoute(mux *http.ServeMux, publicMatcher *http.ServeMux, patterns map[string]bool, r *http.Request) bool {
	if mux != nil {
		_, pattern := mux.Handler(r)
		return pattern != "" && patterns[pattern]
	}
	if publicMatcher != nil {
		_, pattern := publicMatcher.Handler(r)
		return pattern != ""
	}
	return false
}
