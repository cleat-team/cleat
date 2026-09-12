package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// TestA413NamesTheLimitItHitAndWhichKnobMovesIt is cleat#1332.
//
// This server enforces TWO different body ceilings -- the configurable
// --max-body-size and the compile-time signalMaxBodySize -- and every 413 said
// only "request body too large". A caller learned neither the value nor which
// of the two it was, and the expensive case is not the missing number: it is an
// operator who raises --max-body-size, still gets 413 from /signal, and has
// nothing in the response to suggest that endpoint does not use the flag.
//
// THE ASSERTION IS THE PAIRING, NOT THE PRESENCE OF A NUMBER. Checking only
// that a limit appears would pass against a handler that names the wrong one,
// which is worse than saying nothing: a caller who learns the body names the
// knob will believe it. So each case pins the value AND requires the other
// limit's knob to be absent -- swapping the two helpers at any call site turns
// this red. Verified by doing exactly that; see the PR.
//
// No database: with no tenant on the request and requireAuth false, storeFor
// returns s.store directly, so a mock is enough to reach the body read.
func TestA413NamesTheLimitItHitAndWhichKnobMovesIt(t *testing.T) {
	const generalLimit = 4096

	newAPI := func() *apiServer {
		ms := &mockStore{
			getWorkflowByIDFn: func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
				// Non-terminal, so handleWorkflowUpdate's admission checks pass
				// and execution reaches the body read this test is about.
				return &engine.WorkflowInstance{ID: id, DefName: "d", Status: "running", CreatedAt: time.Now()}, nil
			},
		}
		return &apiServer{store: ms, worker: newTestWorker(ms), maxBodySize: generalLimit}
	}

	body := func(n int) string {
		// Valid JSON of a chosen size: the oversized case must be refused for
		// its SIZE, not for being malformed. A 400 "invalid JSON" would read as
		// a pass to a test that only checked "not 200".
		return `{"signal_name":"s","reason":"r","payload":"` + strings.Repeat("x", n) + `"}`
	}

	call := func(t *testing.T, api *apiServer, method, path string, payload string,
		h func(*apiServer, http.ResponseWriter, *http.Request)) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		h(api, resp, req)
		var decoded map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
		return resp.Code, decoded["error"]
	}

	cases := []struct {
		name    string
		path    string
		limit   int64
		knob    string // must appear
		notKnob string // must NOT appear -- this is what makes the pairing testable
		call    func(*apiServer, http.ResponseWriter, *http.Request)
	}{
		{
			name: "signal", path: "/api/workflows/wf-1/signal",
			limit: signalMaxBodySize, knob: "not changed by --max-body-size", notKnob: "set by --max-body-size",
			call: func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleSignal(w, r, "wf-1") },
		},
		{
			// The endpoint neither the doc nor the constant's own comment named,
			// and the one most likely to be reached: its field is a free-text
			// reason.
			name: "cancel", path: "/api/workflows/wf-1/cancel",
			limit: signalMaxBodySize, knob: "not changed by --max-body-size", notKnob: "set by --max-body-size",
			call: func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleCancel(w, r, "wf-1") },
		},
		{
			name: "update", path: "/api/workflows/wf-1/update/u",
			limit: signalMaxBodySize, knob: "not changed by --max-body-size", notKnob: "set by --max-body-size",
			call: func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleWorkflowUpdate(w, r, "wf-1", "u") },
		},
		{
			// The other ceiling, so the two cannot both be satisfied by one
			// hardcoded sentence.
			name: "start", path: "/api/workflows/d/start",
			limit: generalLimit, knob: "set by --max-body-size", notKnob: "fixed for the signal",
			call: func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleStartWorkflow(w, r, "d") },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAPI()

			// THE CONTROL COMES FIRST. Without it, "an oversized body is 413"
			// passes just as well against a handler that answers 413 to
			// everything -- and that failure mode is exactly what a
			// misconfigured MaxBytesReader produces.
			if code, msg := call(t, api, http.MethodPost, c.path, body(8), c.call); code == 413 {
				t.Fatalf("an %d-byte body was refused with 413 (%q), so the oversized "+
					"assertion below would prove nothing", len(body(8)), msg)
			}

			code, msg := call(t, api, http.MethodPost, c.path, body(int(c.limit)+1024), c.call)
			if code != 413 {
				t.Fatalf("oversized body: got %d %q, want 413", code, msg)
			}
			if want := fmt.Sprintf("%d bytes", c.limit); !strings.Contains(msg, want) {
				t.Errorf("413 body %q does not name the limit it hit (%s).\n\n"+
					"Two ceilings are in play on this server and the response is the only "+
					"place a caller can learn which one refused them.", msg, want)
			}
			if !strings.Contains(msg, c.knob) {
				t.Errorf("413 body %q does not say %q", msg, c.knob)
			}
			if strings.Contains(msg, c.notKnob) {
				t.Errorf("413 body %q contains %q, which belongs to the OTHER limit.\n\n"+
					"Naming the wrong knob is worse than naming none: a caller who learns "+
					"the body identifies the knob will act on it.", msg, c.notKnob)
			}
		})
	}
}

// TestEveryBodyLimitSiteThatRefusesWithA413NamesItsLimit is the completeness
// half, and it exists because the test above can only cover the handlers it
// enumerates.
//
// A new handler with a MaxBytesReader and a bare "request body too large" would
// pass everything above by not being listed. This reads the source instead, so
// the omission is what fails.
//
// It deliberately does NOT require every MaxBytesReader to have a 413 branch.
// Seven sites have none at all -- three in this file and four in api_admin.go
// -- and an oversized body there is a 400 whose text begins "invalid JSON".
// That is a status-code change on live endpoints, tracked separately; asserting
// it here would make this guard fail for a reason it is not about.
func TestEveryBodyLimitSiteThatRefusesWithA413NamesItsLimit(t *testing.T) {
	raw, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("reading server.go: %v", err)
	}
	src := string(raw)

	bare := strings.Count(src, `writeError(w, 413, "request body too large")`)
	if bare != 0 {
		t.Errorf("%d 413 site(s) still write a bare \"request body too large\".\n\n"+
			"Use bodyTooLargeConfigured or bodyTooLargeFixed, whichever matches the "+
			"MaxBytesReader above it.", bare)
	}

	fixed := strings.Count(src, "s.bodyTooLargeFixed(w)")
	configured := strings.Count(src, "s.bodyTooLargeConfigured(w)")
	// A floor per helper rather than on the total: a total is satisfied by the
	// wrong mix, and the whole point is that the two do not get confused.
	if fixed < 3 {
		t.Errorf("found %d bodyTooLargeFixed call(s), want at least 3 "+
			"(signal, cancel, update)", fixed)
	}
	if configured < 5 {
		t.Errorf("found %d bodyTooLargeConfigured call(s), want at least 5", configured)
	}
}
