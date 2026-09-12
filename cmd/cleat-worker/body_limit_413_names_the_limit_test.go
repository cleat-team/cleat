package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os/exec"
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

// TestEveryBoundedBodyGoesThroughTheHelper is the completeness half, and it
// replaces a weaker guard that counted calls to two now-deleted responders.
//
// The old one said, deliberately:
//
//	It deliberately does NOT require every MaxBytesReader to have a 413 branch.
//	Seven sites have none at all [...] That is a status-code change on live
//	endpoints, tracked separately.
//
// cleat#1338 made that change, so the exemption is gone and this asserts the
// stronger property instead: EVERY bounded body goes through decodeBody or
// readBody. That subsumes both halves of the old guard -- a bare "request body
// too large" and a missing 413 arm are both impossible when one function
// writes both -- and it is the thing that stops an eighth handler repeating
// the omission, which seven copies of a hand-written branch could not.
//
// It parses rather than greps. A line-oriented scan cannot tell a call from a
// comment about a call, and this file is full of comments about
// MaxBytesReader.
func TestEveryBoundedBodyGoesThroughTheHelper(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	// git ls-files, not filepath.Glob: a scratch worktree or an editor backup
	// under this directory would otherwise be scanned as if it were the
	// package, and a guard gets MORE permissive as the tree gets messier.
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f != "" && !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("PRECONDITION FAILED: git ls-files matched no non-test Go files")
	}

	allowed := map[string]bool{"decodeBody": true, "readBody": true}
	found := 0
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "MaxBytesReader" {
					return true
				}
				found++
				if !allowed[fn.Name.Name] {
					t.Errorf("%s bounds a request body directly (%s:%d).\n\n"+
						"Use s.decodeJSONBody / s.decodeOptionalJSONBody / s.readBody instead. "+
						"Seven handlers bounded their own bodies and never translated "+
						"*http.MaxBytesError, so an oversized body came back as "+
						"400 \"invalid JSON: http: request body too large\" -- a status saying "+
						"the body was malformed, on a body that was never read. cleat#1338.",
						fn.Name.Name, file, fset.Position(call.Pos()).Line)
				}
				return true
			})
			return true
		})
	}

	// A floor, because "every MaxBytesReader is in the helper" is also
	// satisfied by finding none at all -- which is what a broken scan returns.
	if found < 2 {
		t.Errorf("found %d MaxBytesReader call(s) across %d files, want at least 2 "+
			"(decodeBody and readBody); the scan is not looking at the package",
			found, len(files))
	}
}

// TestEveryBodyLimitNamesTheKnobThatMovesIt is the other half of what the old
// guard did, expressed against the type that replaced the two responders: a
// bodyLimit whose help is empty produces "the limit is N bytes, " and names
// nothing, which is the cleat#1332 defect with extra punctuation.
func TestEveryBodyLimitNamesTheKnobThatMovesIt(t *testing.T) {
	api := &apiServer{maxBodySize: 4096}
	for _, c := range []struct {
		name string
		lim  bodyLimit
	}{
		{"configured", api.configuredBodyLimit()},
		{"signal", signalBodyLimit()},
		{"definition", definitionBodyLimit()},
		{"terminate", terminateBodyLimit()},
	} {
		if c.lim.max <= 0 {
			t.Errorf("%s limit is %d, which bounds nothing", c.name, c.lim.max)
		}
		if strings.TrimSpace(c.lim.help) == "" {
			t.Errorf("%s limit names no knob, so its 413 reads "+
				"\"the limit is %d bytes, \" and tells the caller nothing actionable",
				c.name, c.lim.max)
		}
	}
}
