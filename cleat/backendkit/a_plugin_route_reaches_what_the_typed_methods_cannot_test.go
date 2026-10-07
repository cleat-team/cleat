package backendkit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// TestPluginRouteReachesAPluginRoute is the regression test for cleat#2550.
//
// THE GAP IT CLOSES. Client's typed methods cover /api/*, and CallPlugin posts
// to /api/plugins/{plugin}/{function} -- the HOST-FUNCTION path. Neither reaches
// the routes a plugin mounts itself in RegisterRoutes, which are on the same mux
// and are part of the worker's public surface. plugins/notifications registers
// six, and there is no host-function substitute for the one that found this: it
// registers send_webhook and nothing that lists deliveries, so a delivery log
// exists only as an HTTP route.
//
// The test asserts the three things a caller needs and would otherwise hand-roll
// and get wrong one at a time: the request goes to the path it named, it rides
// the CLIENT'S OWN transport (so the API key and the timeout apply), and a
// non-2xx is refused rather than decoded into a zero value.
func TestPluginRouteReachesAPluginRoute(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deliveries":[{"id":"d1","status":"delivered"}]}`))
	}))
	defer srv.Close()

	c := backendkit.New(srv.URL)
	// The key lives on the TRANSPORT, which is how every other method gets it.
	// If PluginRoute built its own http.Client this assertion fails -- and that
	// is the whole reason the method belongs in the library rather than in each
	// app: a hand-rolled call that reaches for http.DefaultClient sends an
	// unauthenticated request and the only symptom is a 401 much later.
	c.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer test-key")
		return http.DefaultTransport.RoundTrip(r)
	})}

	var out struct {
		Deliveries []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"deliveries"`
	}
	if err := c.PluginRoute(context.Background(), http.MethodGet, "/webhooks/abc/deliveries", nil, &out); err != nil {
		t.Fatalf("PluginRoute: %v", err)
	}

	if gotPath != "/webhooks/abc/deliveries" {
		t.Errorf("the server saw path %q, want the one that was passed", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the client transport's key: PluginRoute did "+
			"not ride c.HTTPClient", gotAuth)
	}
	if len(out.Deliveries) != 1 || out.Deliveries[0].Status != "delivered" {
		t.Errorf("decoded %+v, want the one delivery the server sent", out.Deliveries)
	}
}

// A non-2xx is REFUSED, not decoded. Without this, a plugin that answers 404 for
// a webhook belonging to another tenant would decode into an empty struct and
// the caller would report an empty delivery log rather than a wrong id -- a
// silent wrong answer where the server had named the problem.
func TestPluginRouteRefusesANonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"webhook not found"}`))
	}))
	defer srv.Close()

	c := backendkit.New(srv.URL)
	var out map[string]any
	err := c.PluginRoute(context.Background(), http.MethodGet, "/webhooks/nope/deliveries", nil, &out)
	if err == nil {
		t.Fatal("a 404 was accepted; the caller would read an empty result as a real answer")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to name the status", err)
	}
	if out != nil {
		t.Errorf("out was populated (%+v) despite the refusal", out)
	}
}

// A path that is not rooted is refused BEFORE a request is made, and the message
// says why.
//
// `c.BaseURL + "webhooks/abc"` is a valid URL that resolves against the WRONG
// base -- the base's last segment is dropped -- so the failure would be a
// request to an unrelated path on the worker, surfacing as a confusing 404
// rather than as a bad argument.
func TestPluginRouteRefusesAnUnrootedPath(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()

	c := backendkit.New(srv.URL)
	err := c.PluginRoute(context.Background(), http.MethodGet, "webhooks/abc", nil, nil)
	if err == nil {
		t.Fatal("an unrooted path was accepted")
	}
	if reached {
		t.Error("the request was sent anyway: the check has to be before the call, " +
			"not a validation of the response")
	}
	if !strings.Contains(err.Error(), "begin with") {
		t.Errorf("error = %v, want it to say what is wrong with the path", err)
	}
}

// A body is marshalled and typed as JSON; a nil out drains rather than leaking
// the connection.
func TestPluginRouteSendsABodyAndToleratesANilOut(t *testing.T) {
	var gotMethod, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := backendkit.New(srv.URL)
	if err := c.PluginRoute(context.Background(), http.MethodPut, "/rate-limits/edge",
		map[string]any{"max_requests": 50}, nil); err != nil {
		t.Fatalf("PluginRoute with a body: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody["max_requests"] != float64(50) {
		t.Errorf("body = %+v, want the marshalled value", gotBody)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
