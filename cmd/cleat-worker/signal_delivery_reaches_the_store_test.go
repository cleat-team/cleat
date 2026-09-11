package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The signal handler delivers the id, name and payload it was GIVEN.
//
// Found by sabotage survey while scoping cleat#1121: replace each
// request-supplied value the handler reads with a constant, run the package,
// and see whether anything goes red. All three were unguarded.
//
//	handleSignal value    a signal test noticed
//	target id             no
//	signal_name           no
//	payload               no
//
// Twelve tests in this package have "Signal" in their name and every one is
// about allowed-signals AUTHORIZATION -- who may send -- not about delivery.
// So nothing asserted that a signal sent to wf-A for "approve" did not arrive
// at wf-B as "reject", and the name of a test is the weakest available evidence
// about what it constrains. (The technique, and that phrasing, are from #1248,
// where the same survey over /api/workflows found four of seven filters
// guarded by nothing while a confidently-named test covered the other three.)
//
// This is a prerequisite for cleat#1121 rather than part of it. That issue adds
// an idempotency key to the signal path; adding one on top of a delivery path
// with no test that its arguments arrive would mean changing untested code and
// verifying only the new part.
//
// A NOTE ON THE WHOLE-PACKAGE RUN, because it nearly misread. Sabotaging any of
// the three turned TestTheSweepDeletesOnlyWithTheOverride red -- a retention
// test, unrelated to signals, and green on an unmutated tree. It is not a
// guard: a mutated delivery leaves different rows and an unrelated test counts
// them, which is coupling through the database rather than coverage. Running
// only the signal-named tests is what gave the three clean "no" answers above.
func TestTheSignalHandlerDeliversWhatItWasGiven(t *testing.T) {
	var gotID, gotName, gotPayload string
	var calls int

	ms := &mockStore{}
	ms.deliverSignalFn = func(_ context.Context, id, name, payload string) error {
		calls++
		gotID, gotName, gotPayload = id, name, payload
		return nil
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-target/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{\"amount\":42}"}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	// Exactly once. A handler that delivered twice would satisfy every
	// assertion below, and a duplicate delivery is the subject of cleat#1121 --
	// so the count is the one thing this must not leave unstated.
	if calls != 1 {
		t.Fatalf("DeliverSignal called %d times, want exactly 1", calls)
	}
	if gotID != "wf-target" {
		t.Errorf("delivered to %q, want wf-target -- the id comes from the URL path and "+
			"nothing asserted it reached the store", gotID)
	}
	if gotName != "approve" {
		t.Errorf("delivered signal %q, want approve -- a handler that ignored signal_name "+
			"would wake the wrong await and no test in this package would say so", gotName)
	}
	if gotPayload != `{"amount":42}` {
		t.Errorf("delivered payload %q, want {\"amount\":42}.\n\n"+
			"The payload passes through engine.Redact before the store; this one has "+
			"nothing redactable in it, so an exact match is the right assertion and a "+
			"difference means the handler lost or rewrote it.", gotPayload)
	}
}

// A signal for a workflow the caller does not own must not reach the store at
// all -- the partner assertion, and the one that keeps the first honest.
//
// Without it, a handler that passed its arguments through perfectly while
// ignoring ownership would satisfy every check above. callerOwnsTarget answers
// 404 rather than 403, so an unknown id and a foreign one are indistinguishable
// (3.86); what was missing is anything asserting the store is never asked.
//
// AUTHENTICATED, and that is not incidental. The first version of this test used
// the plain unauthenticated server and failed: 200, delivered. Not a defect --
// callerOwnsTarget returns early when the request carries no tenant, because
// "no tenant" means --require-auth=false and there are no tenants to keep
// apart. It never reached GetWorkflowByID, so the stub standing in for "not
// yours" was never consulted. The check is conditional on authentication and a
// test of it has to authenticate.
func TestASignalForAnUnownedWorkflowNeverReachesTheStore(t *testing.T) {
	api, storeA, _, _ := twoTenantServer(t, true)

	var calls int
	storeA.deliverSignalFn = func(context.Context, string, string, string) error {
		calls++
		return nil
	}
	// Tenant A asks about a workflow its own scoped store does not have.
	storeA.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) {
		return nil, nil
	}

	req := asTenant(httptest.NewRequest(http.MethodPost, "/api/workflows/not-mine/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{}"}`)), tenantA)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 -- and 404 rather than 403 on purpose, since 403 "+
			"would confirm the workflow exists", rec.Code)
	}
	if calls != 0 {
		t.Errorf("DeliverSignal was called %d time(s) for a workflow the caller does not "+
			"own; it must not be reached at all", calls)
	}
}

// A key-respecting fake, modelling the constraint the real stores enforce: one
// token delivers once.
//
// Deliberately NOT a mock that returns alreadyDelivered unconditionally while
// ignoring its key. #1167 found two tests built that way -- replacing the
// handler's header read with a constant left both green -- so "the header is
// actually read" was under test nowhere. A mock that ignores the value it is
// handed cannot fail for a wrong one.
//
// Empty keys are not recorded, which is the same distinction the stores make:
// "" is the ABSENCE of a token, so two keyless callers must not collide.
type signalKeyRecorder struct {
	spent      map[string]bool
	deliveries int
	keysSeen   []string
}

func newKeyedSignalServer() (*apiServer, *signalKeyRecorder) {
	rec := &signalKeyRecorder{spent: map[string]bool{}}
	ms := &mockStore{}
	ms.deliverSignalIdempotentFn = func(_ context.Context, _, _, _, key string) (bool, error) {
		rec.keysSeen = append(rec.keysSeen, key)
		if key != "" && rec.spent[key] {
			return true, nil
		}
		if key != "" {
			rec.spent[key] = true
		}
		rec.deliveries++
		return false, nil
	}
	ms.deliverSignalFn = func(context.Context, string, string, string) error {
		// The keyless path does not reach DeliverSignalIdempotent, so it is
		// counted here -- otherwise "two keyless sends deliver twice" would
		// assert against a counter nothing increments.
		rec.keysSeen = append(rec.keysSeen, "")
		rec.deliveries++
		return nil
	}
	return newTestAPIServer(ms), rec
}

func postSignal(api *apiServer, id, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/"+id+"/signal",
		strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)
	return rec
}

// TestARetriedSignalWithTheSameKeyIsAbsorbed — cleat#1121 at the HTTP edge.
func TestARetriedSignalWithTheSameKeyIsAbsorbed(t *testing.T) {
	api, ks := newKeyedSignalServer()

	first := postSignal(api, "wf-1", "tok-a")
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"delivered"`) {
		t.Fatalf("first send: %d %s", first.Code, first.Body.String())
	}

	retry := postSignal(api, "wf-1", "tok-a")
	if retry.Code != 200 {
		t.Fatalf("retry status %d, want 200", retry.Code)
	}
	if !strings.Contains(retry.Body.String(), "already_delivered") {
		t.Errorf("retry answered %s, want already_delivered.\n\n"+
			"A bare \"delivered\" is indistinguishable from having delivered a second "+
			"time, which is the ambiguity the key exists to remove.", retry.Body.String())
	}
	if ks.deliveries != 1 {
		t.Errorf("the store delivered %d time(s) for one token, want 1", ks.deliveries)
	}

	// A different key delivers again: idempotency is not a once-ever rule.
	other := postSignal(api, "wf-1", "tok-b")
	if !strings.Contains(other.Body.String(), `"delivered"`) || ks.deliveries != 2 {
		t.Errorf("a second key answered %s with %d total deliveries, want delivered and 2",
			other.Body.String(), ks.deliveries)
	}
}

// TestTheHeaderIsReadRatherThanAssumed — the assertion #1167 found missing.
//
// A handler that passed a constant, or dropped the header entirely, would
// satisfy every status assertion above: the first send is "delivered" either
// way. Only the key the STORE was handed can tell.
func TestTheHeaderIsReadRatherThanAssumed(t *testing.T) {
	api, ks := newKeyedSignalServer()

	postSignal(api, "wf-1", "tok-from-the-header")
	if len(ks.keysSeen) != 1 || ks.keysSeen[0] != "tok-from-the-header" {
		t.Fatalf("the store was handed %v, want [tok-from-the-header]", ks.keysSeen)
	}
}

// TestTwoKeylessSignalsDoNotCollide — "" is an absence, not a token.
//
// If the handler treated a missing header as the key "", the second keyless
// caller would be absorbed as a duplicate of the first: a signal silently never
// delivered, from a client that asked for no idempotency at all.
func TestTwoKeylessSignalsDoNotCollide(t *testing.T) {
	api, ks := newKeyedSignalServer()

	for i := 0; i < 2; i++ {
		rec := postSignal(api, "wf-1", "")
		if !strings.Contains(rec.Body.String(), `"delivered"`) {
			t.Fatalf("keyless send %d answered %s", i, rec.Body.String())
		}
	}
	if ks.deliveries != 2 {
		t.Errorf("two keyless sends produced %d delivery/deliveries, want 2", ks.deliveries)
	}
}
