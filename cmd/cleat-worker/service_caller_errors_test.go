package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The engine has honoured engine.RetryableError since long before this file
// existed -- isDefinitelyNonRetryable consults it to decide whether to keep
// retrying, and IMPROVEMENT-PLAN 2.35 made the resulting classification
// survive into the event history so replay agrees with the first run.
//
// None of that did anything for the shipped worker, because dbServiceCaller
// returned bare fmt.Errorf for every failure. Every error looked equally
// retryable, so a workflow calling a service that is not configured at all
// burned its full retry budget on a deployment mistake and was then told the
// call was retryable, so a workflow with its own retry wrapper would go round
// again. Forever.

// retryabilityOf reports what the engine would conclude about err.
//
// It calls the same predicate DurableCallWithRetry uses rather than asserting
// on the concrete type: the question these tests care about is not "is this a
// *CleatError" but "will the engine stop retrying", and those are only the
// same thing for as long as the plumbing between them holds.
func retryabilityOf(err error) bool {
	var re engine.RetryableError
	if errors.As(err, &re) {
		return re.Retryable()
	}
	// Nothing self-reported: the engine treats it as worth retrying.
	return true
}

func TestUnconfiguredServiceIsNotRetryable(t *testing.T) {
	c := &dbServiceCaller{}
	_, err := c.Call(context.Background(), "billing", "charge", `{}`)
	if err == nil {
		t.Fatal("calling an unconfigured service succeeded")
	}
	if retryabilityOf(err) {
		t.Error("a service with no endpoint registered is reported as retryable; " +
			"retrying a deployment mistake cannot fix it, and the workflow burns its " +
			"whole retry budget before failing anyway")
	}
	// The message is load-bearing: operators grep for it, and
	// DurableCallWithRetry's nonRetryableErrors patterns match on substrings.
	if want := "service billing.charge not configured"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q no longer contains %q", err, want)
	}
}

func TestHTTPFetchMalformedRequestIsNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
		want    string
	}{
		{"invalid JSON", `{not json`, "invalid request JSON"},
		{"missing url", `{"method":"GET"}`, "url is required"},
		{"bad method", `{"url":"http://example.com","method":"NOT A METHOD"}`, "invalid request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &dbServiceCaller{}
			_, err := c.Call(context.Background(), "http", "fetch", tc.request)
			if err == nil {
				t.Fatal("expected an error")
			}
			if retryabilityOf(err) {
				t.Errorf("%s is reported as retryable; the same bytes will be rejected the same way every time", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestHTTPFetchNetworkFailureStaysRetryable is the other half, and the one
// that would be quietly broken by an over-eager classifier. A connection that
// could not be established is the textbook retryable failure; marking it
// permanent would turn every transient blip into a failed workflow.
func TestHTTPFetchNetworkFailureStaysRetryable(t *testing.T) {
	// A server that is closed before the call, so dialling reliably fails
	// without depending on DNS or the network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := &dbServiceCaller{}
	_, err := c.Call(context.Background(), "http", "fetch", fmt.Sprintf(`{"url":%q}`, url))
	if err == nil {
		t.Fatal("expected a connection failure")
	}
	if !retryabilityOf(err) {
		t.Errorf("a failed connection is reported as non-retryable (%v); "+
			"that turns every transient blip into a failed workflow", err)
	}
}

// TestHTTPFetchStatusIsNotAnError pins behaviour this change must not alter.
// http.fetch reports the status code in its response rather than as an error,
// so a 404 is a successful call that returned 404 -- classifying it would be
// classifying something that is not a failure.
func TestHTTPFetchStatusIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	c := &dbServiceCaller{}
	resp, err := c.Call(context.Background(), "http", "fetch", fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil {
		t.Fatalf("a 404 response became a call error: %v", err)
	}
	if !strings.Contains(resp, `"status":404`) {
		t.Errorf("response %q does not carry the status code", resp)
	}
}

func TestBenchSvcStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
		why       string
	}{
		{http.StatusBadRequest, false, "bench-svc understood the request and rejected it"},
		{http.StatusNotFound, false, "the route does not exist"},
		{http.StatusRequestTimeout, true, "408 is an explicit invitation to try again"},
		{http.StatusTooManyRequests, true, "429 is an explicit invitation to try again"},
		{http.StatusInternalServerError, true, "5xx may well succeed on a retry"},
		{http.StatusBadGateway, true, "5xx may well succeed on a retry"},
	} {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			err := benchSvcStatusError(tc.status, []byte("body"))
			if got := retryabilityOf(err); got != tc.retryable {
				t.Errorf("status %d: retryable=%v, want %v (%s)", tc.status, got, tc.retryable, tc.why)
			}
		})
	}
}

// The other half of the chain -- that the engine actually acts on this -- is
// asserted in engine/callerrors_test.go
// (TestNonRetryableCallIsNotReportedAsRetryable,
// TestFreshAndReplayAgreeOnNonRetryableFailure), which drives the real
// DurableCallWithRetry. It is not duplicated here: doing so would need
// exported test-only helpers on the engine, and a public API that exists only
// to let another package's tests reach inside is a worse trade than two tests
// that meet in the middle at engine.RetryableError -- which is exactly what
// retryabilityOf above pins.
