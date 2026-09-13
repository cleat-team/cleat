package main

// cleat#1224, the half of it that survived.
//
// The issue as filed asked for enumeration. That was already decided against --
// cleat#1119, closed at the owner's direction, and docs/how-to/common-patterns.md
// says the HTTP API is keyed-only BY DESIGN. What is left is the opposite
// change: if the key is a required argument, omitting it is a 400, not a 200
// with an empty value.
//
// The handler's own comment already said an empty value carries "three
// meanings, one response". An absent ?key= was a fourth, and the only one the
// design explicitly forbids.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAQueryWithNoKeyIsA400(t *testing.T) {
	queried := false
	ms := existingWorkflow(&mockStore{})
	ms.getQueryStateFn = func(_ context.Context, _, _ string) (string, error) {
		queried = true
		return "", nil
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleGetQueryState(rec,
		httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/query", nil), "wf-1")

	if rec.Code != 400 {
		t.Errorf("a query with no key answered %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if queried {
		t.Error("the store was asked for a key the caller never supplied")
	}
}

// TestAnEmptyKeyIsStillALookup is the control, and it is the one that stops the
// fix from over-reaching.
//
// "" is a STORABLE key: nothing validates it on the write path
// (HostCallsImpl.SetQueryState -> set_query_state, both pass it through), and
// jsonb holds it -- '{"":"v"}'::jsonb ->> ” is 'v'. So ?key= is a legitimate
// request for that key, and rejecting it would make the key unreachable.
//
// Without this case, `if key == ""` would pass the test above and silently
// break the only way to read a key published as "".
func TestAnEmptyKeyIsStillALookup(t *testing.T) {
	var askedFor string
	gotAsked := false
	ms := existingWorkflow(&mockStore{})
	ms.getQueryStateFn = func(_ context.Context, _, k string) (string, error) {
		askedFor, gotAsked = k, true
		return "published-under-empty", nil
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleGetQueryState(rec,
		httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/query?key=", nil), "wf-1")

	if rec.Code != 200 {
		t.Fatalf("?key= answered %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !gotAsked {
		t.Fatal("?key= did not reach the store; an empty key is a lookup, not a malformed request")
	}
	if askedFor != "" {
		t.Errorf("the store was asked for %q, want the empty key", askedFor)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("undecodable body %q: %v", rec.Body.String(), err)
	}
	if body["value"] != "published-under-empty" {
		t.Errorf("value = %q, want the stored value", body["value"])
	}
}

// TestANormalKeyStillWorks is the second control: the 400 must not fire on the
// ordinary path, which is how every real caller uses this endpoint.
func TestANormalKeyStillWorks(t *testing.T) {
	ms := existingWorkflow(&mockStore{})
	ms.getQueryStateFn = func(_ context.Context, _, k string) (string, error) {
		if k != "counter" {
			t.Errorf("store asked for %q, want %q", k, "counter")
		}
		return "7", nil
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleGetQueryState(rec,
		httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/query?key=counter", nil), "wf-1")

	if rec.Code != 200 {
		t.Fatalf("?key=counter answered %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["key"] != "counter" || body["value"] != "7" {
		t.Errorf("body = %v, want key=counter value=7", body)
	}
}
