package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3377: cleat_fetch on a stock worker, behind --enable-workflow-fetch.
//
// Every refusal below goes through the real dbServiceCaller.guardedFetch and
// the real engine.EgressGuard, with no network: a refused destination is
// refused before any connection is attempted.

// allowEveryHost is a tenant policy that permits anything, so that a refusal in
// the tests below can only have come from the floor underneath it.
func allowEveryHost(context.Context, string) (bool, error) { return true, nil }

func TestWorkflowFetchIsOffUnlessTheFlagIsSet(t *testing.T) {
	caller := &dbServiceCaller{}

	if f := (&Worker{}).workflowFetcherFor(caller); f != nil {
		t.Errorf("a worker without --enable-workflow-fetch installed a Fetcher (%T)", f)
	}

	f := (&Worker{workflowFetch: true}).workflowFetcherFor(caller)
	wf, ok := f.(*workflowFetcher)
	if !ok {
		t.Fatalf("with the flag set, got %T, want *workflowFetcher", f)
	}
	if wf.maxBody != defaultWorkflowFetchMaxBody {
		t.Errorf("an unset cap resolved to %d, want the default %d", wf.maxBody, defaultWorkflowFetchMaxBody)
	}

	f = (&Worker{workflowFetch: true, workflowFetchMaxBody: 4096}).workflowFetcherFor(caller)
	if got := f.(*workflowFetcher).maxBody; got != 4096 {
		t.Errorf("the configured cap was %d, want 4096", got)
	}
}

func TestWorkflowFetchRefusesTheFloorEvenWhenTheTenantAllowsEveryHost(t *testing.T) {
	// No AllowLoopback: this is the production shape of the guard, with only the
	// tenant layer opened wide.
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowHost: allowEveryHost}}
	f := &workflowFetcher{caller: c, maxBody: defaultWorkflowFetchMaxBody}

	for _, tc := range []struct{ url, why string }{
		{"http://169.254.169.254/latest/meta-data/", "link-local: cloud instance metadata"},
		{"http://10.0.0.5/internal", "RFC1918"},
		{"http://192.168.1.1/", "RFC1918"},
		{"http://127.0.0.1:8080/admin", "the worker's own admin surface"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			_, err := f.Fetch(context.Background(), "GET", tc.url, "{}", "")
			if err == nil {
				t.Fatalf("fetching %s succeeded; it must be refused (%s)", tc.url, tc.why)
			}
			if !strings.Contains(err.Error(), "network policy") {
				t.Errorf("refused, but not by the egress policy -- %q", err)
			}
			if !notRetryable(t, err) {
				t.Errorf("a policy refusal must not be retryable: %v", err)
			}
		})
	}
}

func TestWorkflowFetchNeedsTheTenantsOwnPermissionToo(t *testing.T) {
	// A worker with the flag on and a tenant that has not been given any host.
	// The destination is a public name that is never dialled, because the
	// refusal comes first -- so a pass here cannot be about the network.
	c := &dbServiceCaller{}
	f := &workflowFetcher{caller: c, maxBody: defaultWorkflowFetchMaxBody}

	_, err := f.Fetch(context.Background(), "GET", "https://example.com/", "{}", "")
	if err == nil {
		t.Fatal("a tenant with no allowlist fetched a public host; an empty list permits nothing")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("refused, but not for want of an allowlist -- %q", err)
	}
}

func TestWorkflowFetchReturnsTheResponseWhenTheGuardPermits(t *testing.T) {
	var gotMethod, gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotHeader, gotBody = r.Method, r.Header.Get("X-Probe"), string(b)
		w.Header().Set("X-Reply", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)

	// AllowLoopback because the listener is on 127.0.0.1 and the floor correctly
	// refuses it; the same idiom as the idempotency-key test beside this one.
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}
	f := &workflowFetcher{caller: c, maxBody: defaultWorkflowFetchMaxBody}

	out, err := f.Fetch(context.Background(), "POST", srv.URL, `{"X-Probe":"p1"}`, `{"q":1}`)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if gotMethod != "POST" || gotHeader != "p1" || gotBody != `{"q":1}` {
		t.Errorf("the server saw method=%q X-Probe=%q body=%q", gotMethod, gotHeader, gotBody)
	}
	var resp struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("the response is not the http.fetch JSON: %v\n%s", err, out)
	}
	if resp.Status != http.StatusTeapot || resp.Body != "hello" || resp.Headers["X-Reply"] != "yes" {
		t.Errorf("response = %+v, want status 418, body hello, X-Reply yes", resp)
	}
}

func TestWorkflowFetchNamesTheCapWhenTheResponseIsLarger(t *testing.T) {
	const capBytes = 64
	var size int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", size)))
	}))
	t.Cleanup(srv.Close)
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}
	f := &workflowFetcher{caller: c, maxBody: capBytes}

	size = capBytes // exactly the cap is allowed: the boundary, not just the far side
	if _, err := f.Fetch(context.Background(), "GET", srv.URL, "{}", ""); err != nil {
		t.Fatalf("a body of exactly the cap was refused: %v", err)
	}

	size = capBytes + 1
	_, err := f.Fetch(context.Background(), "GET", srv.URL, "{}", "")
	if err == nil {
		t.Fatal("a body one byte over the cap was returned; it must fail, not be cut")
	}
	for _, want := range []string{strconv.Itoa(capBytes), "--workflow-fetch-max-response-bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
}

// The DurableCall("http", "fetch") path shares guardedFetch and must keep its
// old behaviour: a larger body is cut at 1 MiB and returned, not refused.
func TestHTTPFetchStillTruncatesInsteadOfFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", httpFetchMaxBody+10)))
	}))
	t.Cleanup(srv.Close)
	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}

	out, err := c.handleHTTPFetch(context.Background(), fetchRequestJSON(t, srv.URL, nil), "")
	if err != nil {
		t.Fatalf("http.fetch now fails on an oversized body: %v", err)
	}
	var resp struct{ Body string }
	_ = json.Unmarshal([]byte(out), &resp)
	if len(resp.Body) != httpFetchMaxBody {
		t.Errorf("body length = %d, want it cut to %d", len(resp.Body), httpFetchMaxBody)
	}
}

func TestWorkflowFetchRejectsHeadersThatAreNotAJSONObject(t *testing.T) {
	f := &workflowFetcher{caller: &dbServiceCaller{}, maxBody: defaultWorkflowFetchMaxBody}
	_, err := f.Fetch(context.Background(), "GET", "https://example.com/", `["not","an","object"]`, "")
	if err == nil || !strings.Contains(err.Error(), "headers must be a JSON object") {
		t.Errorf("err = %v, want one saying headers must be a JSON object", err)
	}
}
