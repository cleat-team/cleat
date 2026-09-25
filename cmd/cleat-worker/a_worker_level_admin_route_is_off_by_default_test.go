package main

// Every /api/admin/ route is absent until --enable-admin-api is set (cleat#2267).
//
// Measured by cleat-review on a live worker while reviewing #2263: POST /api/admin/drain answered 202 to
// an ordinary tenant's key, so any tenant could take workers out of rotation, and GET /api/admin/health
// returned other plugins' messages. The flag that gated force-complete, force-fail and re-replay did not
// gate them, because the gate lived inside two handlers and the others were registered bare. A handler
// added later inherits nothing by omission, so the test reads the registrations rather than a list.
//
// Three checks, each with the case that could make it pass while measuring nothing beside it:
//
//   - the registrations are read from app.go's syntax tree (a comment that mentions a route is not one),
//     and the scan must find the three routes known to exist, or it read nothing;
//   - with the flag off, every /api/admin/ route answers the 404 an unregistered /api/ path gets, for a
//     request carrying a tenant, and it never reaches the SPA;
//   - with the flag on the same routes are reached, so the 404 above is the gate and not a missing route.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

type adminRegistration struct {
	file, pattern string
	gated         bool
	inRegister    bool
}

// adminRegistrations finds every mux registration of a /api/admin/ pattern in the package's non-test
// files, and whether its handler is wrapped in adminAPIOnly.
func adminRegistrations(t *testing.T) []adminRegistration {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []adminRegistration
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				pat := strings.Trim(lit.Value, "`\"")
				if i := strings.Index(pat, " "); i >= 0 { // "POST /api/..."
					pat = pat[i+1:]
				}
				if !strings.HasPrefix(pat, "/api/admin") {
					return true
				}
				gated := false
				if wrap, ok := call.Args[1].(*ast.CallExpr); ok {
					if ws, ok := wrap.Fun.(*ast.SelectorExpr); ok && ws.Sel.Name == "adminAPIOnly" {
						gated = true
					}
				}
				out = append(out, adminRegistration{file: f, pattern: pat, gated: gated, inRegister: fn.Name.Name == "registerRoutes"})
				return true
			})
		}
	}
	return out
}

func adminMux(api *apiServer) *http.ServeMux {
	return registerRoutes(http.NewServeMux(), api)
}

func adminTestServer() *apiServer {
	w := newTestWorker(&mockStore{})
	w.drainCh = make(chan struct{}) // a drain that completes closes it
	return &apiServer{
		store:  &mockStore{},
		worker: w,
		spa: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(spaMarker))
		}),
	}
}

func withAdminAPI(t *testing.T, on bool) {
	t.Helper()
	old := enableAdminAPI
	enableAdminAPI = &on
	t.Cleanup(func() { enableAdminAPI = old })
}

// A probe path for a registered pattern: a subtree pattern gets a leaf under it.
func probePath(pattern string) string {
	if strings.HasSuffix(pattern, "/") {
		return pattern + "wf-1/force-complete"
	}
	return pattern
}

func TestEveryAdminRouteIsAbsentUntilTheAdminAPIIsEnabled(t *testing.T) {
	regs := adminRegistrations(t)

	// Vacuity: the three worker-level and tenant-scoped families that exist today. A scan that found fewer
	// read nothing, and would pass whatever app.go registered.
	seen := map[string]bool{}
	for _, r := range regs {
		seen[r.pattern] = true
	}
	for _, want := range []string{"/api/admin/drain", "/api/admin/retention/sweep", "/api/admin/instances/"} {
		if !seen[want] {
			t.Fatalf("the scan of the registrations did not find %s: it read nothing, or the route moved (found %v)", want, seen)
		}
	}

	// Structure: registered in registerRoutes, and wrapped.
	for _, r := range regs {
		if !r.inRegister {
			t.Errorf("%s registers %s outside registerRoutes: there is one route table (route_table_test.go)", r.file, r.pattern)
		}
		if !r.gated {
			t.Errorf("%s is registered without adminAPIOnly: it would answer any tenant's key with --enable-admin-api off (cleat#2267)", r.pattern)
		}
	}

	// Behaviour, with the flag off. The comparison body is what an unregistered /api/ path answers.
	withAdminAPI(t, false)
	api := adminTestServer()
	mux := adminMux(api)
	tenant := uuid.New()
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req = req.WithContext(auth.WithTenantID(req.Context(), tenant))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	missing := do(http.MethodGet, "/api/no/such/endpoint")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("the control: an unregistered /api/ path answered %d, want 404", missing.Code)
	}
	for _, r := range regs {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := do(method, probePath(r.pattern))
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), spaMarker) {
				t.Errorf("[flag off, tenant key] %s %s = %d %q, want 404 as if the route did not exist", method, probePath(r.pattern), rec.Code, rec.Body.String())
				continue
			}
			if strings.TrimSpace(rec.Body.String()) != strings.TrimSpace(missing.Body.String()) {
				t.Errorf("[flag off] %s %s answered %q, an unregistered path answers %q: the gate is distinguishable from a missing route",
					method, probePath(r.pattern), rec.Body.String(), missing.Body.String())
			}
		}
	}

	// The known-positive: the same requests with the flag on reach the handlers, so the 404s above were the
	// gate and not routes that were never registered.
	withAdminAPI(t, true)
	if rec := do(http.MethodPost, "/api/admin/drain"); rec.Code != http.StatusAccepted {
		t.Errorf("[flag on] POST /api/admin/drain = %d %q, want 202: the route is not reachable when enabled", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/api/admin/drain"); rec.Code != http.StatusOK {
		t.Errorf("[flag on] GET /api/admin/drain = %d, want 200", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/retention/sweep", strings.NewReader(`{"older_than":"-1h"}`))
	req = req.WithContext(auth.WithTenantID(req.Context(), tenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		body, _ := io.ReadAll(rec.Body)
		t.Errorf("[flag on] POST /api/admin/retention/sweep with a bad window = %d %q, want 400 from the handler", rec.Code, body)
	}
}

// The warning that goes with the flag says what turning it on means, and is silent when it is off.
func TestTheAdminAPIWarningNamesTheExposure(t *testing.T) {
	if got := adminAPIExposure(false, true); got != "" {
		t.Errorf("with the flag off there is nothing to warn about, got %q", got)
	}
	on := adminAPIExposure(true, true)
	for _, want := range []string{"any authenticated API key", "drain", "cleat#2169"} {
		if !strings.Contains(on, want) {
			t.Errorf("the warning %q does not mention %q", on, want)
		}
	}
	open := adminAPIExposure(true, false)
	if !strings.Contains(open, "EVERYONE") || open == on {
		t.Errorf("with --require-auth=false the warning must say it is open to everyone, got %q", open)
	}
}
