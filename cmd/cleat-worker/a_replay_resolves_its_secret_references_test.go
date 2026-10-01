package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestReplayUnderOriginalKeyResolvesItsSecretReference covers cleat#2911 G1
// (ii): ReplayUnderOriginalKey re-dispatches the ORIGINAL request, which may
// carry a ${secret:name} reference exactly like a first dispatch does --
// before this fix, it skipped resolveSecrets entirely and sent the literal,
// unresolved reference text to the service. nilDBSecretStore makes every
// lookup fail, which is the observable signal that resolution was ATTEMPTED:
// an implementation that did not resolve would pass the reference through
// and send the request, getting a 200 back with no error at all. It is the
// same device TestADurableCallResolvesItsSecretReference and
// TestBothCallEntryPointsResolve use for Call/CallWithIdempotencyKey.
func TestReplayUnderOriginalKeyResolvesItsSecretReference(t *testing.T) {
	var gotRequests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&gotRequests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ops, err := parseIdempotencyKeyOps("stripe.charge")
	if err != nil {
		t.Fatalf("parseIdempotencyKeyOps: %v", err)
	}
	c := &dbServiceCaller{
		secrets:           nilDBSecretStore(t),
		serviceEndpoints:  map[string]string{"stripe": srv.URL},
		idempotencyKeyOps: ops,
		egress:            allowLoopbackForTest(),
	}

	const req = `{"headers":{"Authorization":"${secret:stripe_key}"}}`
	resp, outcome, err := c.ReplayUnderOriginalKey(tenantCtx(), "stripe", "charge", req, "key-1")

	if err == nil {
		t.Fatal("a request carrying ${secret:...} returned no error against a store " +
			"that cannot look anything up; the reference was not resolved, which is " +
			"the defect this test exists to catch")
	}
	if !strings.Contains(err.Error(), "stripe_key") {
		t.Errorf("error does not name the unresolvable secret, so an operator cannot "+
			"tell which reference failed: %v", err)
	}
	if outcome != engine.IdempotencyReplayCannotSay {
		t.Errorf("outcome = %v, want IdempotencyReplayCannotSay on a resolution failure", outcome)
	}
	if resp != "" {
		t.Errorf("response = %q, want empty", resp)
	}
	if n := atomic.LoadInt32(&gotRequests); n != 0 {
		t.Errorf("service received %d request(s), want 0 -- a resolution failure must "+
			"dispatch nothing rather than send the literal, unresolved reference", n)
	}
}

// TestAWorkerWithoutSecretsLeavesReplayRequestsAlone mirrors
// TestAWorkerWithoutSecretsLeavesRequestsAlone for the replay entry point: a
// deployment started with no CLEAT_SECRET_MASTER_KEY has a nil secrets
// store, and ReplayUnderOriginalKey must still dispatch normally.
func TestAWorkerWithoutSecretsLeavesReplayRequestsAlone(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ops, err := parseIdempotencyKeyOps("stripe.charge")
	if err != nil {
		t.Fatalf("parseIdempotencyKeyOps: %v", err)
	}
	c := &dbServiceCaller{
		serviceEndpoints:  map[string]string{"stripe": srv.URL},
		idempotencyKeyOps: ops,
		egress:            allowLoopbackForTest(),
	}

	const req = `{"k":"${secret:x}"}`
	resp, outcome, err := c.ReplayUnderOriginalKey(tenantCtx(), "stripe", "charge", req, "key-1")
	if err != nil {
		t.Fatalf("nil secret store must be a pass-through: %v", err)
	}
	if outcome != engine.IdempotencyReplayResolved {
		t.Errorf("outcome = %v, want IdempotencyReplayResolved", outcome)
	}
	if resp != `{"ok":true}` {
		t.Errorf("response = %q, want the service's body", resp)
	}
	if gotBody != req {
		t.Errorf("request body = %q, want %q unchanged (no secret store to resolve against)", gotBody, req)
	}
}
