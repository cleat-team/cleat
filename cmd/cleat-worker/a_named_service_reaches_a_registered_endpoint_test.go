package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// allowLoopbackForTest supplies the guard a test server needs.
//
// The egress floor refuses loopback and is deliberately not configurable, so a
// caller pointed at httptest cannot dial it without an operator exemption. This
// builds the exempted guard directly rather than reaching for a flag, so the
// test says what it is relying on.
func allowLoopbackForTest() *engine.EgressGuard {
	return &engine.EgressGuard{
		OperatorAllows: func(_ context.Context, _ string) (bool, error) { return true, nil },
		AllowHost:      func(_ context.Context, _ string) (bool, error) { return true, nil },
		AllowLoopback:  true,
	}
}

// TestANamedServiceReachesItsRegisteredEndpoint drives the whole path a workflow
// takes to a microservice that is not a plugin: lookup, route, headers, body.
//
// The route and the headers are asserted together because they are what the
// service on the other end has to agree with, and they are the part a
// refactor would silently change. A test that only asserted "a request
// arrived" would pass against a forwarder that posted to the wrong path with
// no idempotency key, which is the shape this mechanism exists to provide.
func TestANamedServiceReachesItsRegisteredEndpoint(t *testing.T) {
	var gotPath, gotKey, gotTrace, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("Idempotency-Key")
		gotTrace = r.Header.Get("traceparent")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"charged":true}`))
	}))
	defer srv.Close()

	c := &dbServiceCaller{
		serviceEndpoints: map[string]string{"billing": srv.URL},
		egress:           allowLoopbackForTest(),
		traceID:          "0af7651916cd43dd8448eb211c80319c",
	}

	resp, err := c.CallWithIdempotencyKey(context.Background(),
		"billing", "charge", `{"amount":100}`, "key-abc")
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	if resp != `{"charged":true}` {
		t.Errorf("response = %q", resp)
	}
	if gotPath != "/call/billing/charge" {
		t.Errorf("path = %q, want /call/billing/charge -- the service routes on this", gotPath)
	}
	if gotKey != "key-abc" {
		t.Errorf("Idempotency-Key = %q; without it the service cannot deduplicate a "+
			"retried at-least-once call", gotKey)
	}
	if !strings.Contains(gotTrace, "0af7651916cd43dd8448eb211c80319c") {
		t.Errorf("traceparent = %q, want the caller's trace id", gotTrace)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody != `{"amount":100}` {
		t.Errorf("body = %q", gotBody)
	}
}

// TestARegisteredEndpointWinsOverTheCatchAll pins the precedence, which is what
// makes this change safe for a deployment already using --bench-svc-url: adding
// a registration changes only the service it names.
func TestARegisteredEndpointWinsOverTheCatchAll(t *testing.T) {
	var reachedNamed, reachedCatchAll bool
	named := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedNamed = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer named.Close()
	catchAll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedCatchAll = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer catchAll.Close()

	c := &dbServiceCaller{
		serviceEndpoints: map[string]string{"billing": named.URL},
		benchSvcURL:      catchAll.URL,
		egress:           allowLoopbackForTest(),
	}

	if _, err := c.Call(context.Background(), "billing", "charge", `{}`); err != nil {
		t.Fatalf("registered service: %v", err)
	}
	if !reachedNamed || reachedCatchAll {
		t.Errorf("registered=%v catchAll=%v; the registration must win", reachedNamed, reachedCatchAll)
	}

	reachedNamed, reachedCatchAll = false, false
	if _, err := c.Call(context.Background(), "unregistered", "op", `{}`); err != nil {
		t.Fatalf("unregistered service: %v", err)
	}
	if reachedNamed || !reachedCatchAll {
		t.Errorf("registered=%v catchAll=%v; an unregistered service must still fall "+
			"through to --bench-svc-url", reachedNamed, reachedCatchAll)
	}
}

// TestAnUnregisteredServiceSaysHowToRegisterOne: the refusal is the only thing
// a reader has, and "not configured" alone sends them to write a plugin they
// may not need.
func TestAnUnregisteredServiceSaysHowToRegisterOne(t *testing.T) {
	c := &dbServiceCaller{egress: allowLoopbackForTest()}

	_, err := c.Call(context.Background(), "billing", "charge", `{}`)
	if err == nil {
		t.Fatal("want an error for a service with no endpoint")
	}
	if !strings.Contains(err.Error(), "--service-endpoints billing=") {
		t.Errorf("the refusal does not name the way out: %v", err)
	}
	var ce *engine.CleatError
	if !errors.As(err, &ce) || ce.Code != engine.ErrPermanent {
		t.Errorf("an unregistered service is not permanent, so the engine will retry a "+
			"call that cannot start succeeding: %v", err)
	}
}

// The arm that makes the rest of this file safe to have written.
//
// A registered endpoint is operator configuration, so it is tempting to treat it
// as trusted and skip the guard -- which is exactly what the forwarder did
// before cleat#1841, when it held a bare &http.Transport{}. It must not: a
// registry entry is a string, and it can name the cloud metadata address as
// easily as a billing service.
//
// THE FIRST VERSION OF THIS TEST ASSERTED THE WRONG THING. It required a
// registered endpoint on loopback to be refused, on the reasoning that the floor
// applies to everything. The floor does still apply -- but "refuse every private
// address" deletes the feature rather than securing it, because a registered
// endpoint is almost always private: a sidecar on loopback, or a cluster
// name that is RFC1918 by construction. Five tests in tests/crash failed on
// exactly that. cleat#1841 settled it: the exemption is the endpoint being
// dialled, and it cannot reach a range the table marks non-exemptible.
//
// So the floor is pinned where it is absolute, which is where it matters.
func TestARegisteredEndpointIsStillSubjectToTheEgressFloor(t *testing.T) {
	// Registered, and still refused: checkAddr tests the RANGE before the host,
	// so naming a host cannot turn the metadata address into a grant.
	c := &dbServiceCaller{serviceEndpoints: map[string]string{"billing": "http://169.254.169.254"}}

	_, err := c.Call(context.Background(), "billing", "charge", `{}`)
	if err == nil {
		t.Fatal("a registered endpoint reached the cloud metadata address; " +
			"registering a host must never grant this range")
	}
	if !strings.Contains(err.Error(), "cannot be exempted") {
		t.Errorf("refused with %q, want it to say the range cannot be exempted -- an "+
			"operator who registered this endpoint would otherwise read the bare floor "+
			"message as their configuration not having arrived", err)
	}
}

// And the other half of that settlement, which is the deployment shape: a
// registered endpoint on loopback is reachable, because that is what a sidecar
// is. This is the case the first version of the test above forbade.
func TestARegisteredEndpointOnLoopbackIsReachable(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// No egress field, so the caller builds its own guard -- the production one,
	// not a loopback-exempted test guard. That is the point: this must work
	// without a test-only escape hatch.
	c := &dbServiceCaller{serviceEndpoints: map[string]string{"billing": srv.URL}}

	if _, err := c.Call(context.Background(), "billing", "charge", `{}`); err != nil {
		t.Fatalf("a registered endpoint on loopback was refused: %v -- this is the "+
			"ordinary deployment (a sidecar, or a cluster-internal name), and refusing "+
			"it deletes the feature rather than securing it", err)
	}
	if !reached {
		t.Error("the call reported success without reaching the server")
	}
}

// A GUEST NAMES A SERVICE, NOT A HOST, which is why the exemption above is safe
// to give at all. The workflow supplies "billing"; the operator supplies the
// URL. A name the operator never registered resolves to nothing and is never
// dialled, so there is no way to spend the exemption on a host of one's own
// choosing.
func TestAGuestCannotReachAHostTheOperatorDidNotRegister(t *testing.T) {
	c := &dbServiceCaller{serviceEndpoints: map[string]string{"billing": "http://127.0.0.1:1"}}

	_, err := c.Call(context.Background(), "http://169.254.169.254", "charge", `{}`)
	if err == nil {
		t.Fatal("a service name that was never registered produced a call")
	}
	// The refusal must come from the registry, not from the dialler: nothing
	// should have been dialled at all.
	if strings.Contains(err.Error(), "network policy") {
		t.Errorf("refused at the egress guard (%q), meaning an unregistered name was "+
			"turned into a destination and dialled; it must not reach that far", err)
	}
}
