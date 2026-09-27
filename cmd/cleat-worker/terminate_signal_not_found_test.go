package main

// IMPROVEMENT-PLAN 3.101. Two routes answered something other than 404 for a
// workflow the caller does not own, and the two answers were different from
// each other and from what an unknown id produced.
//
//	POST /api/dead-letters/{id}/terminate   200 "terminated", having done nothing
//	POST /api/workflows/{id}/signal        500 "Violation of PRIMARY KEY ..."
//
// THE 500 IS NOT WHAT THIS TEST SHOWS, and the distinction is worth stating.
// It was measured against a live SQL Server while 3.86 was being written: the
// MERGE's ON clause is tenant-scoped, so a foreign id falls through to the
// INSERT branch and pk_workflow_signals refuses it. The mock here does not
// simulate that, so reverting this fix shows a 200 rather than a 500. What
// this test asserts is the HANDLER property -- 404, store not reached, and the
// same body for "not yours" as for "does not exist" -- which is true whatever
// the store would have gone on to do.
//
// Neither was a leak. 3.86 put `AND tenant_id` on the terminate's UPDATE and on
// DeliverSignal's MERGE, so nothing crossed. What crossed was INFORMATION ABOUT
// WHICH IDS ARE REAL: an unknown id produced a different response from a real
// one belonging to somebody else, so the routes answered a question the caller
// was not entitled to ask. cmd/cleat-worker/api_admin.go had already written
// this down for the admin API -- "It answers 404, never 403: 403 would confirm
// that the workflow exists" -- and these two routes predate that reasoning.
//
// So the assertions here are mostly about SAMENESS rather than about a status
// code. A test that only checked for 404 would pass against an implementation
// that answered 404 for a foreign id and 404-with-a-different-body for an
// unknown one, which is still an oracle.
//
// They also assert THE STORE WAS NOT REACHED. Checking the status code alone is
// too weak in the same way TestAdminRoutesRejectCrossTenantTarget records: a
// handler can answer 404 after having already performed the operation.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

const (
	tsCallerTenant = "11111111-1111-4111-8111-111111111111"
	tsOtherTenant  = "22222222-2222-4222-8222-222222222222"
	tsWorkflowID   = "wf-ownership-probe"
)

type tsRoute struct {
	name, prefix, path, body string
	arm                      func(*mockStore, *bool)
}

func tsRoutes() []tsRoute {
	return []tsRoute{
		{
			name: "terminate", prefix: "/api/dead-letters/", path: "terminate", body: `{"reason":"x"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.terminateWorkflowFn = func(context.Context, string, string) error {
					*reached = true
					return nil
				}
			},
		},
		{
			name: "signal", prefix: "/api/workflows/", path: "signal", body: `{"signal_name":"go","payload":"{}"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.deliverSignalFn = func(context.Context, string, string, string) error {
					*reached = true
					return nil
				}
			},
		},
		// Added after the fact: cancel was the third route taking an id from
		// the URL path without checking it, and it answered 200
		// "cancellation_requested" for a workflow it had not cancelled. Its two
		// siblings, retry and query-state, turned out not to need the check --
		// see the comment on handleCancel for why.
		{
			name: "cancel", prefix: "/api/workflows/", path: "cancel", body: `{"reason":"x"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.requestCancellationFn = func(context.Context, string, string) error {
					*reached = true
					return nil
				}
			},
		},
	}
}

// tsRequest issues one call as tsCallerTenant against a workflow owned by
// ownedBy. An empty ownedBy means the workflow does not exist at all.
func tsRequest(t *testing.T, rt tsRoute, ownedBy string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	ms := &mockStore{}
	rt.arm(ms, &reached)
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		if ownedBy == "" {
			return nil, nil
		}
		return &engine.WorkflowInstance{ID: id, TenantID: ownedBy}, nil
	}

	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost,
		rt.prefix+tsWorkflowID+"/"+rt.path, strings.NewReader(rt.body))
	req = req.WithContext(auth.WithTenantID(req.Context(), uuid.MustParse(tsCallerTenant)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w, reached
}

func TestTerminateAndSignalAnswerNotFoundForWorkflowsTheCallerDoesNotOwn(t *testing.T) {
	for _, rt := range tsRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			// Owner: the operation must still work. Without this, a handler
			// that 404s unconditionally passes everything below.
			okResp, okReached := tsRequest(t, rt, tsCallerTenant)
			if okResp.Code != http.StatusOK {
				t.Fatalf("owner got %d, want 200 -- the route is refusing its own tenant: %s",
					okResp.Code, okResp.Body.String())
			}
			if !okReached {
				t.Errorf("owner's request never reached the store, so the 200 means nothing")
			}

			foreign, foreignReached := tsRequest(t, rt, tsOtherTenant)
			if foreign.Code != http.StatusNotFound {
				t.Errorf("another tenant's workflow got %d, want 404: %s",
					foreign.Code, foreign.Body.String())
			}
			if foreignReached {
				t.Errorf("the store was reached for another tenant's workflow; the status "+
					"code says %d but the operation was applied", foreign.Code)
			}

			missing, missingReached := tsRequest(t, rt, "")
			if missing.Code != http.StatusNotFound {
				t.Errorf("unknown id got %d, want 404: %s", missing.Code, missing.Body.String())
			}
			if missingReached {
				t.Errorf("the store was reached for an id that does not exist")
			}

			// The assertion this file exists for. Both are 404 above; if they
			// differ in body, the route still answers "this id is real".
			if foreign.Body.String() != missing.Body.String() {
				t.Errorf("a workflow owned by another tenant and an id that does not exist "+
					"produce DIFFERENT responses, so this route is an oracle for which ids "+
					"are real:\n  foreign: %s\n  missing: %s",
					strings.TrimSpace(foreign.Body.String()),
					strings.TrimSpace(missing.Body.String()))
			}
		})
	}
}
