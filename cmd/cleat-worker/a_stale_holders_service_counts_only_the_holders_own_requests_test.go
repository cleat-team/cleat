package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// cleat#3711. TestAReclaimedRunIsNotClobberedByItsStaleHolderUnderRLS failed once in
// four whole-package runs with 5,471 "dispatches after release": /api/tasks,
// /api/tasks/result and /api/agent/poll, in a hot loop, and none of the test's own ops.
// The test's service is an httptest server on an ephemeral port and it recorded the path
// of EVERY request it received, so a client this test did not start, sending to that port,
// failed an assertion about the stale holder. (Who sent them is not established; clew#514
// is the probable cause.)
//
// The assertion is about the holder, so the service counts only the holder's own requests,
// which reach it as /call/<op>, and answers anything else 404 and keeps it apart. This
// reproduces the original condition against the real service type: a hot loop of
// another client's paths after the release, with the holder's genuine late call as the
// known-positive, so a service that stopped counting the holder's own late calls is as
// visible as one that counted a stranger's.
func TestAStaleHoldersServiceCountsOnlyTheHoldersOwnRequests(t *testing.T) {
	svc := &staleHolderService{}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// Before the release: the holder's own call, and a stray among them.
	if got := get("/call/work/first"); got != http.StatusOK {
		t.Fatalf("the holder's own call was answered %d, want 200", got)
	}
	get("/api/agent/poll")

	svc.released.Store(true)

	// After the release: the original condition, one poll, one task fetch and one result
	// post per loop, then the holder's genuine late call.
	var wantStrays []string
	for i := 0; i < 5; i++ {
		for _, p := range []string{"/api/agent/poll", "/api/tasks", "/api/tasks/result"} {
			if got := get(p); got != http.StatusNotFound {
				t.Errorf("a request that is not the holder's was answered %d, want 404", got)
			}
			wantStrays = append(wantStrays, p)
		}
	}
	get("/call/work/second")

	arrivals, strays := svc.snapshot()

	var after []string
	var all []string
	for _, a := range arrivals {
		all = append(all, a.op)
		if a.after {
			after = append(after, a.op)
		}
	}
	if !reflect.DeepEqual(all, []string{"work/first", "work/second"}) {
		t.Errorf("the service counted %v as the holder's requests, want [work/first work/second]: a client that is "+
			"not the holder was counted as the holder (cleat#3711)", all)
	}
	// The known-positive: the holder's genuine call after the release IS reported.
	if !reflect.DeepEqual(after, []string{"work/second"}) {
		t.Errorf("after the release the service reported %v, want [work/second]", after)
	}
	// And the strays are visible, not hidden.
	if len(strays) != 1+len(wantStrays) || strays[0] != "/api/agent/poll" {
		t.Errorf("the service kept %d stray path(s) %v, want %d, so a foreign client is visible", len(strays), strays, 1+len(wantStrays))
	}
}

func TestHolderOwnOpRecognisesOnlyCallPaths(t *testing.T) {
	for path, want := range map[string]struct {
		op  string
		own bool
	}{
		"/call/work/first":  {"work/first", true},
		"/call/work/second": {"work/second", true},
		"/call/":            {"", false},
		"/call":             {"", false},
		"/api/tasks":        {"", false},
		"/api/agent/poll":   {"", false},
		"/":                 {"", false},
	} {
		if op, own := holderOwnOp(path); op != want.op || own != want.own {
			t.Errorf("holderOwnOp(%q) = (%q, %v), want (%q, %v)", path, op, own, want.op, want.own)
		}
	}
}
