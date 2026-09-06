package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The two tests here guard one property from both sides: the routes the binary
// serves are the routes this package registers, and there is one place that
// registers them.
//
// What they were written against: main() built its own mux inline and never
// called registerRoutes, which was reached only from StartAPIServer -- a
// function nothing but tests called. So `/api/instances/...` and every
// `/api/admin/instances/...` route existed on the tested table and on no
// served one. Seven endpoints, unreachable since the file was written.
//
// The SPA catch-all is why that cost nothing to notice and everything to find:
// it answered any unmatched path with 200 and index.html, so the missing routes
// returned success and HTML rather than 404. A guard that only checked status
// codes would have passed against the broken server.

// spaMarker is what the SPA handler returns in these tests. An API path that
// comes back holding it reached the catch-all, which means no route claimed it.
const spaMarker = "SPA-CATCH-ALL"

// apiRoutesTheBinaryMustServe is every API path that must resolve to a real
// handler. Paths, not patterns: the point is what a client can reach.
//
// The instance and admin entries are the seven that were unreachable. The rest
// are here so that a future edit which drops one is caught by the same test
// rather than by whoever was relying on it.
var apiRoutesTheBinaryMustServe = []struct{ method, path string }{
	{http.MethodGet, "/healthz"},
	{http.MethodGet, "/metrics"},
	{http.MethodGet, "/api/workflows"},
	{http.MethodGet, "/api/workflows/wf-1"},
	{http.MethodGet, "/api/schedules"},
	{http.MethodGet, "/api/schedules/s-1"},
	{http.MethodGet, "/api/dead-letters"},
	{http.MethodGet, "/api/dead-letters/wf-1"},
	{http.MethodGet, "/api/definitions"},
	{http.MethodPost, "/api/definitions"},
	{http.MethodPost, "/api/admin/drain"},

	// Instance inspection -- api_instances.go.
	{http.MethodGet, "/api/instances/wf-1"},
	{http.MethodGet, "/api/instances/wf-1/state"},
	{http.MethodGet, "/api/instances/wf-1/events"},

	// Admin operations -- api_admin.go. CHANGELOG.md documents re-replay as a
	// shipped operator feature.
	{http.MethodPost, "/api/admin/instances/wf-1/force-complete"},
	{http.MethodPost, "/api/admin/instances/wf-1/force-fail"},
	{http.MethodPost, "/api/admin/instances/wf-1/re-replay"},
	{http.MethodPost, "/api/admin/instances/wf-1/steps/0/resolve"},
}

func TestEveryAPIRouteResolvesToAHandlerAndNotTheSPA(t *testing.T) {
	mux := http.NewServeMux()
	api := &apiServer{
		store:  &mockStore{},
		worker: newTestWorker(&mockStore{}),
		spa: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(spaMarker))
		}),
	}
	registerRoutes(mux, api)

	for _, rt := range apiRoutesTheBinaryMustServe {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// ServeMux.Handler reports which pattern claims the request,
			// which is the question here -- not what the handler then does
			// with an empty store. A pattern of "/" is the catch-all.
			req := httptest.NewRequest(rt.method, rt.path, nil)
			_, pattern := mux.Handler(req)
			if pattern == "/" || pattern == "" {
				t.Fatalf("%s %s is served by the catch-all (pattern %q), so no route claims it.\n"+
					"Register it in registerRoutes (cmd/cleat-worker/app.go). Do NOT register it "+
					"in main(): one table is the property this test exists to hold.",
					rt.method, rt.path, pattern)
			}

			// And prove it behaviourally, because a pattern that matches but
			// whose handler falls through to the SPA would be the same defect
			// wearing the right name.
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if strings.Contains(rec.Body.String(), spaMarker) {
				t.Fatalf("%s %s reached the SPA catch-all despite matching pattern %q",
					rt.method, rt.path, pattern)
			}
		})
	}
}

func TestAnUnmatchedAPIPathIs404AndNotTheSPA(t *testing.T) {
	mux := http.NewServeMux()
	api := &apiServer{
		store:  &mockStore{},
		worker: newTestWorker(&mockStore{}),
		spa: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(spaMarker))
		}),
	}
	registerRoutes(mux, api)

	// An /api/ path nothing claims must be a 404, so that a client built
	// against a route the server does not have is told so. This is the half
	// that made the missing routes invisible: they answered 200 with HTML.
	req := httptest.NewRequest(http.MethodGet, "/api/no/such/endpoint", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unmatched /api/ path: got %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), spaMarker) {
		t.Error("unmatched /api/ path was answered with the SPA")
	}

	// A non-API path still gets the SPA -- the wrapper must not break the web
	// UI's client-side routing, which depends on unknown paths returning it.
	req = httptest.NewRequest(http.MethodGet, "/workflows/wf-1", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), spaMarker) {
		t.Errorf("non-API path did not reach the SPA: %d %q", rec.Code, rec.Body.String())
	}
}

// muxRegistration matches a route registration on a mux.
//
// Comments are stripped before this runs. Nothing in main.go currently mentions
// a registration in prose, so the stripping is not load-bearing today -- it is
// here because a scanner that reads a sentence ABOUT a thing as the thing is a
// mistake this repository has made four times, most recently in the guard for
// #827, where the explanatory comment named the helper the test was looking for
// and backing the fix out left the test green. TestTheScannerIgnoresComments
// below is what keeps the stripping honest rather than decorative.
var muxRegistration = regexp.MustCompile(`\bmux\.(Handle|HandleFunc)\(|\bRegisterVersionHandler\(`)

func TestOnlyRegisterRoutesRegistersRoutes(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := stripGoComments(string(src))
	if loc := muxRegistration.FindStringIndex(body); loc != nil {
		line := 1 + strings.Count(body[:loc[0]], "\n")
		t.Fatalf("main.go registers a route itself (stripped-source line %d: %q).\n"+
			"Every route belongs in registerRoutes (app.go). A second table is how "+
			"/api/instances and /api/admin/instances came to be registered where the "+
			"tests could see them and the binary could not.",
			line, strings.TrimSpace(body[loc[0]:min(loc[0]+60, len(body))]))
	}
}

// stripGoComments removes // and /* */ comments, leaving string literals and
// line structure intact.
func stripGoComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	const (
		code = iota
		lineComment
		blockComment
		inString
		inRawString
		inRune
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				i++
			default:
				out.WriteByte(c)
				switch c {
				case '"':
					state = inString
				case '`':
					state = inRawString
				case '\'':
					state = inRune
				}
			}
		case lineComment:
			if c == '\n' {
				out.WriteByte(c)
				state = code
			}
		case blockComment:
			if c == '\n' {
				// Keep newlines so reported line numbers stay usable.
				out.WriteByte(c)
			}
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			}
		case inString, inRune:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if (state == inString && c == '"') || (state == inRune && c == '\'') {
				state = code
			}
		case inRawString:
			out.WriteByte(c)
			if c == '`' {
				state = code
			}
		}
	}
	return out.String()
}

// TestTheScannerIgnoresComments exercises stripGoComments directly, so the
// stripping above is a tested property rather than an untested precaution. A
// commented-out or merely discussed registration must not trip the scan; a real
// one, including one inside a string, must.
func TestTheScannerIgnoresComments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		match bool
	}{
		{"real registration", "func f() {\n\tmux.HandleFunc(\"/x\", h)\n}\n", true},
		{"commented out", "func f() {\n\t// mux.HandleFunc(\"/x\", h)\n}\n", false},
		{"discussed in prose", "// main() no longer calls mux.HandleFunc itself.\nfunc f() {}\n", false},
		{"inside a block comment", "/*\nmux.Handle(\"/x\", h)\n*/\nfunc f() {}\n", false},
		{"version handler", "func f() {\n\tRegisterVersionHandler(mux, s)\n}\n", true},
		{"version handler in prose", "// See RegisterVersionHandler( for why.\nfunc f() {}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := muxRegistration.MatchString(stripGoComments(tc.src))
			if got != tc.match {
				t.Errorf("stripGoComments+match = %v, want %v, for:\n%s", got, tc.match, tc.src)
			}
		})
	}
}
