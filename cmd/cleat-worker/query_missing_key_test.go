package main

// cleat#1224, then cleat#1571.
//
// cleat#1224 asked for enumeration and got the opposite: cleat#1119 had decided
// published state was keyed-only BY DESIGN, so an absent ?key= became a 400
// rather than a 200 with an empty value.
//
// cleat#1571 REVERSES the design half, on the operational case cleat#1119 itself
// named -- "a run misbehaved and you do not know what it published" -- and on
// the owner's framing that query state is a semantically limited standard
// interface, so viewing it belongs to that interface. An absent ?key= now
// lists.
//
// WHAT DID NOT CHANGE is the distinction below, and it is the subtle half: `?key=`
// is a lookup of the key published as "", not a request to list. Both tests are
// kept because one without the other permits the wrong fix.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAQueryWithNoKeyListsEverythingPublished(t *testing.T) {
	singleKeyRead := false
	ms := existingWorkflow(&mockStore{})
	ms.getQueryStateFn = func(_ context.Context, _, _ string) (string, error) {
		singleKeyRead = true
		return "", nil
	}
	ms.listQueryStateFn = func(_ context.Context, _ string) (map[string]string, error) {
		return map[string]string{"a": "1", "": "published-under-empty"}, nil
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleGetQueryState(rec,
		httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/query", nil), "wf-1")

	if rec.Code != 200 {
		t.Fatalf("a query with no key answered %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if singleKeyRead {
		t.Error("the store was asked for a single key the caller never supplied")
	}
	var body struct {
		State map[string]string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("undecodable body %q: %v", rec.Body.String(), err)
	}
	if body.State["a"] != "1" {
		t.Errorf("state = %v, want the published keys", body.State)
	}
	// The key published as "" must appear in a listing. It is the one key the
	// single-key reader makes awkward to ask for, so enumeration is where it
	// becomes discoverable at all.
	if _, ok := body.State[""]; !ok {
		t.Error("the key published as \"\" is missing from the listing, which is the " +
			"one place it can be discovered")
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
