package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// notRetryable reports whether err is classified so the executor will not try
// again. Asserted through Retryable() rather than the Code constant because
// Retryable() is the behaviour: a refusal that retries burns the attempt
// budget against an answer that cannot change.
func notRetryable(t *testing.T, err error) bool {
	t.Helper()
	var ce *engine.CleatError
	if !errors.As(err, &ce) {
		t.Errorf("error is not a *engine.CleatError, so it carries no retry "+
			"classification at all: %v", err)
		return false
	}
	return !ce.Retryable()
}

// cleat#1565, end to end through the path a guest actually takes: a workflow
// asks http.fetch for a forbidden destination and is refused.
//
// NOT a source scan. A grep for engine.EgressGuard in setup.go would pass with
// the transport assigned to a client nothing uses, and the whole defect this
// fixes is that a plausible-looking request path did not inspect its
// destination. These call the handler.
//
// No network is needed and none is used: every address below is refused before
// a connection is attempted, which is itself the property under test.
func TestAWorkflowCannotFetchTheHostsOwnNetwork(t *testing.T) {
	c := &dbServiceCaller{}
	ctx := context.Background()

	for _, tc := range []struct{ url, why string }{
		{"http://169.254.169.254/latest/meta-data/", "cloud instance metadata -- the headline case"},
		{"http://[::ffff:169.254.169.254]/latest/meta-data/", "the same, IPv4-mapped"},
		{"http://127.0.0.1:8080/admin", "the worker's own admin surface"},
		{"http://localhost:8080/admin", "the same by name; the check is on the RESOLVED address"},
		{"http://10.0.0.5/internal", "RFC1918"},
		{"http://192.168.1.1/", "RFC1918"},
		{"http://[fd00::1]/", "IPv6 unique local"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			req, _ := json.Marshal(map[string]string{"url": tc.url, "method": "GET"})
			_, err := c.handleHTTPFetch(ctx, string(req))
			if err == nil {
				t.Fatalf("fetching %s succeeded; it must be refused (%s)", tc.url, tc.why)
			}
			if !strings.Contains(err.Error(), "network policy") {
				t.Errorf("refused, but not by the egress policy -- the error is %q, which "+
					"means something else rejected it and the policy is untested here", err)
			}
			// PERMANENT, not transient. A refusal that retries burns the
			// attempt budget against an answer that cannot change.
			if !notRetryable(t, err) {
				t.Errorf("the refusal of %s is retryable; a policy decision must not "+
					"be retried (cleat#1565 open question 5)", tc.url)
			}
		})
	}
}

// The other direction, so the test above cannot pass by refusing everything --
// a policy that denies every destination satisfies every deny assertion.
//
// A permitted destination is fetched end to end and must SUCCEED. The first
// version of this pointed at a name reserved by RFC 2606 and skipped when the
// environment resolved it anyway; scripts/check-skips.sh rejected that, and
// rightly: the error then came from DNS rather than from the policy, so it was
// asserting on the absence of a substring in a message produced by something
// else. This needs no network and cannot skip.
func TestAPermittedDestinationIsFetchedNotRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := &dbServiceCaller{egress: &engine.EgressGuard{AllowLoopback: true}}
	req, _ := json.Marshal(map[string]string{"url": srv.URL, "method": "GET"})
	resp, err := c.handleHTTPFetch(context.Background(), string(req))
	if err != nil {
		t.Fatalf("a permitted destination was refused: %v", err)
	}
	if !strings.Contains(resp, `"status":200`) || !strings.Contains(resp, "hello") {
		t.Errorf("the fetch did not round-trip; response was %q", resp)
	}
}

func TestAGuestCannotChooseANonHTTPScheme(t *testing.T) {
	c := &dbServiceCaller{}
	for _, u := range []string{"file:///etc/passwd", "gopher://example.com/", "ftp://example.com/"} {
		req, _ := json.Marshal(map[string]string{"url": u, "method": "GET"})
		_, err := c.handleHTTPFetch(context.Background(), string(req))
		if err == nil {
			t.Errorf("%s was accepted", u)
			continue
		}
		if !notRetryable(t, err) {
			t.Errorf("%s was refused retryably; a bad scheme cannot become good", u)
		}
	}
}
