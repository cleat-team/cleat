package main

// cleat#3799. A route that addresses a run asks first whether the run's definition is `internal`, and the
// question starts with a read of the run. refuseIfInternalRun used to answer "not refused" when that read
// FAILED, on the reasoning that the handler reads the run again and produces its own miss. That holds for a
// handler that does read it again. It does not hold for signal and cancel, which go straight to the action, so
// with the run unreadable the guard let the request through whatever the run was.
//
// The property this pins is the one the guard exists for: a check that cannot find out must not serve. It
// fails closed on an unreadable run exactly as it already does on an unreadable definition class
// (cleat#3785): a 503 with Retry-After, the error logged, nothing done.
//
// WHERE IT BITES: callerOwnsTarget, which runs first, reads the run itself whenever the request carries a tenant,
// and answers its own 500 when that read fails -- so on the authenticated path an unreadable run never reaches
// the guard. With NO tenant in the request context (a worker without --require-auth) callerOwnsTarget returns
// without reading, and the guard's read is the only one before the action. That is the case driven first below;
// the tenant case is kept as the control that shows the other layer holding.
//
// Every run-addressed route in the two tables is driven with the run unreadable and the definition INTERNAL, so
// that serving the request is the wrong answer on both counts. Which of them go through refuseIfInternalRun
// (the function that changed) is read off the source rather than listed here, so a route added to the tables
// and guarded that way is held to the 503 without anyone remembering to add it.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// driveWithoutATenant is drive (that file) with no tenant in the request context, which is what a worker started
// without --require-auth serves. A deadline for the same reason drive has one.
func driveWithoutATenant(t *testing.T, entry apiEntry, api *apiServer, r externalRoute) *httptest.ResponseRecorder {
	t.Helper()
	body := r.body
	if body == "" && (r.method == http.MethodPost || r.method == http.MethodPut) {
		body = `{}`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(r.method, r.path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	entry(api, rec, req)
	return rec
}

// handlersCalling returns the handlers, by name, whose body calls the named guard.
func handlersCalling(t *testing.T, guard string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range parsed.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == guard {
						found[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	return found
}

func TestARunThatCannotBeReadIsNeverServed(t *testing.T) {
	plain := handlersCalling(t, "refuseIfInternalRun")
	// Known positives: a scan that found nothing, or missed these two, would pass every row below vacuously.
	for _, must := range []string{"handleSignal", "handleCancel"} {
		if !plain[must] {
			t.Fatalf("the source scan did not find %s calling refuseIfInternalRun (found %v); the test would hold nothing to the 503", must, plain)
		}
	}

	driven := 0
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			if r.collection || !strings.Contains(r.path, "/RUN") {
				continue // only the run-addressed routes read a run
			}
			for _, withTenant := range []bool{false, true} {
				withTenant := withTenant
				variant := "no tenant"
				if withTenant {
					variant = "tenant"
				}
				t.Run(table.name+"/"+r.name+"/"+variant, func(t *testing.T) {
					logs := capturedLog(t)
					ms := storeServing(engine.ExposureInternal, true)
					fail := errors.New(driverError)
					ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return nil, fail }
					ms.getTerminalRunFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return nil, fail }
					var rec *httptest.ResponseRecorder
					if withTenant {
						rec = drive(t, entry, newTestAPIServer(ms), r)
					} else {
						rec = driveWithoutATenant(t, entry, newTestAPIServer(ms), r)
					}
					driven++

					// Whatever guards it, an unreadable run is not served: a 2xx or a 404 here is the fail-open (a 404
					// is also the wrong answer for an error, which is what cleat#3785 changed).
					if rec.Code < 500 {
						t.Fatalf("%s answered %d %s with the run unreadable and its definition internal; want a 5xx",
							r.handler, rec.Code, strings.TrimSpace(rec.Body.String()))
					}
					// With a tenant, callerOwnsTarget reads the run first and answers its own 500; only the tenant-less
					// request reaches the guard's read, which is the one that changed.
					if withTenant || !plain[r.handler] {
						return
					}
					if rec.Code != http.StatusServiceUnavailable {
						t.Errorf("%s calls refuseIfInternalRun and answered %d %s; want 503, which tells a client to retry",
							r.handler, rec.Code, strings.TrimSpace(rec.Body.String()))
					}
					if rec.Header().Get("Retry-After") == "" {
						t.Errorf("%s: 503 without a Retry-After", r.handler)
					}
					if strings.Contains(rec.Body.String(), driverError) {
						t.Errorf("%s put the driver's error in the response body:\n%s", r.handler, rec.Body.String())
					}
					if !strings.Contains(logs.String(), driverError) {
						t.Errorf("%s swallowed the error: nothing was logged.\nlog: %q", r.handler, logs.String())
					}
				})
			}
		}
	}
	if driven == 0 {
		t.Fatal("no run-addressed route was driven")
	}
}
