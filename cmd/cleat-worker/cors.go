package main

import "net/http"

// corsAllowMethods is a fixed, generous method list for every allowed
// origin -- the worker's API is not route-specific about which methods a
// browser-based caller might need, and gating per-route would need the
// mux's own registered method set at the point this middleware runs, which
// it does not have (it wraps the whole handler, not one route).
const corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"

// corsDefaultAllowHeaders is what a preflight gets back when the browser's
// own Access-Control-Request-Headers is absent -- malformed by the spec,
// but cheap to answer sensibly rather than refuse. Authorization is here
// because the worker's own auth is a bearer token in that header; Content-Type
// because most of this API's bodies are JSON.
const corsDefaultAllowHeaders = "Authorization, Content-Type"

// newCORSMiddleware returns http middleware implementing cleat#2345's
// worker CORS allowlist: exact origins only, off by default.
//
// OFF BY DEFAULT, EXACT ORIGINS ONLY, NEVER "*" WITH CREDENTIALS -- all
// three are the same property, enforced by construction rather than by a
// runtime check. An empty allowedOrigins returns the identity middleware
// (wraps nothing), so a deployment that never configures this gets
// byte-identical behavior to before this existed. A non-empty list is
// matched by exact string equality against the request's Origin header --
// there is no wildcard matching anywhere in this file, and "*" is never
// written to Access-Control-Allow-Origin; the only value that header is
// ever set to is the caller's own, already-matched Origin. So the unsafe
// combination ("*" plus a credentialed request) has no code path that
// could produce it. See this function's own doc comment on
// Access-Control-Allow-Credentials, below, for why that header is never
// set at all.
//
// OUTERMOST -- wire this as the LAST wrap in main.go, around every other
// middleware including auth, not merely before it. Two independent reasons,
// both about requests this file's own refusal logic must not interfere
// with:
//
//  1. A browser's CORS PREFLIGHT (OPTIONS with Access-Control-Request-Method
//     set) carries no Authorization header and must be answered without
//     ever reaching auth -- auth.MiddlewareWithMux would 401 it before this
//     function's own answer (204, or 403 for a disallowed origin) ever had
//     a chance to run, if this were installed inside auth rather than
//     around it.
//  2. For an ORDINARY (non-preflight) request, the browser enforces CORS on
//     the RESPONSE, not the request: a 401 from auth with no
//     Access-Control-Allow-Origin header is invisible to the page's own
//     JavaScript regardless of HTTP status -- the fetch/XHR call itself
//     reports a generic network error, not "401". So the header has to be
//     set whether the INNER handler (auth included) succeeds or fails,
//     which requires wrapping outside it, not merely running before it in
//     the same layer.
//
// Access-Control-Allow-Credentials is never set, deliberately. That header
// governs cookies, HTTP basic/digest auth, and TLS client certificates --
// the credential types a browser attaches to a request automatically. This
// worker's own auth is a bearer token the CALLER'S CODE attaches explicitly
// (the Authorization header, present in corsDefaultAllowHeaders above), which
// works cross-origin without it. Setting it would only add risk (letting a
// browser also attach cookies/basic-auth automatically) for a capability
// nothing here uses.
func newCORSMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	if len(allowedOrigins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Unconditional, on every path through this middleware -- cleat-review's
			// finding on #3282. The response varies by Origin on EVERY branch below,
			// including the two that set no Access-Control-* header at all (a refused
			// preflight's 403, and an ordinary request from a disallowed or absent
			// origin): the same URL gets a different response depending on Origin.
			// Setting Vary only on the allowed branches left the other two cacheable
			// by an intermediary (CDN/ingress cache -- nothing in cmd/cleat-worker sets
			// Cache-Control: no-store on these routes) as if Origin did not matter,
			// which could serve one origin's cached response to another's request for
			// the same URL -- the standard CORS cache-poisoning class.
			w.Header().Set("Vary", "Origin")

			// A preflight, not an ordinary OPTIONS request someone's own
			// route happens to register: Access-Control-Request-Method is
			// the signal a browser sends only when it is asking permission
			// before the real request, never something a non-preflight
			// caller would set on its own.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if origin == "" || !allowed[origin] {
					// 403, not 404: the ROUTE is not in question here (this
					// middleware runs before routing even happens), the
					// ORIGIN is what is refused, and 403 says so without
					// implying the path itself is missing. cleat#2345's own
					// scope line allows either; this file picks one and
					// uses it consistently.
					w.WriteHeader(http.StatusForbidden)
					return
				}

				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)

				// Authorization reaches Access-Control-Allow-Headers ONLY
				// on this branch -- the one reachable only for an ALLOWED
				// origin's preflight, per cleat#2345's explicit scope line.
				// A disallowed origin already returned above and never sees
				// this header or any other.
				//
				// Echo the browser's own requested header list when it sent
				// one, rather than a fixed set: a deployment's frontend may
				// need a header this file has no way to anticipate (a
				// tenant header under --tenant-resolver=header:<name>, for
				// one), and echoing is safe here BECAUSE it only happens
				// for an origin this allowlist already matched -- it is not
				// a wildcard grant, it is "whatever this already-trusted
				// caller is asking for".
				if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
					w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
				} else {
					w.Header().Set("Access-Control-Allow-Headers", corsDefaultAllowHeaders)
				}

				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			// Not a preflight. An allowed origin gets the response header
			// every subsequent handler's response (success OR failure, see
			// this function's own doc comment) needs for the browser to let
			// its own JS read it. A disallowed or absent origin gets no
			// special treatment at all -- not refused here, since this is
			// an ordinary (possibly non-browser) request and the browser's
			// own enforcement is what actually blocks a disallowed origin's
			// JS from reading the response; refusing server-side here would
			// also break every non-browser caller that happens to send an
			// Origin header this allowlist does not recognise.
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			next.ServeHTTP(w, r)
		})
	}
}
