package main

// cleat#4041: the version a continue_as_new asked for is the version the next run starts on.
//
// Until the fix the worker passed wf.DefVersion to ContinueAsNew unconditionally, so continue_as_new_versioned was
// accepted, recorded, replayed and then ignored. These tests are on the DECISION (continuationVersion), which is
// where the behaviour is, and run against a mock store so each outcome is reachable: a real database can serve
// "deployed" and "not deployed" but not "the read failed". The end-to-end claim, that a chain on a real worker
// and each dialect's store ends on the requested version, is asserted by the TypeScript real-worker test; the tests
// further down drive executeWorkflow itself, so the call site is covered here too.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

type versionStore struct {
	deployed map[int]bool
	err      error
	asked    []int
}

func (v *versionStore) ValidateVersion(_ context.Context, _ string, version int) (bool, error) {
	v.asked = append(v.asked, version)
	return v.deployed[version], v.err
}

func TestContinuationVersion(t *testing.T) {
	wf := &engine.WorkflowInstance{ID: "r1", DefName: "chain", DefVersion: 1}
	cases := []struct {
		name        string
		requested   int
		deployed    map[int]bool
		readErr     error
		wantVersion int
		wantRefusal string // substring; "" means none
		wantErr     bool
		wantAsked   int // how many times the store was consulted
	}{
		{name: "plain continue stays on the current version", requested: 0, wantVersion: 1},
		{name: "the same version needs no lookup", requested: 1, wantVersion: 1},
		{name: "a deployed version is honoured", requested: 2, deployed: map[int]bool{2: true}, wantVersion: 2, wantAsked: 1},
		{name: "an undeployed version is refused by name", requested: 3, deployed: map[int]bool{2: true}, wantRefusal: `version 3 of "chain"`, wantAsked: 1},
		{name: "a failed read is an error, not a refusal", requested: 2, readErr: errors.New("connection reset"), wantErr: true, wantAsked: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &versionStore{deployed: c.deployed, err: c.readErr}
			v, refusal, err := continuationVersion(context.Background(), st, wf, c.requested)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				if refusal != "" {
					t.Fatalf("a failed read produced a refusal %q: it would fail a run over a transient error", refusal)
				}
				return
			}
			if c.wantRefusal != "" {
				if !strings.Contains(refusal, c.wantRefusal) {
					t.Fatalf("refusal = %q, want it to name %s", refusal, c.wantRefusal)
				}
				if !strings.Contains(refusal, "rather than continued on version 1") {
					t.Errorf("refusal %q does not say it declined to fall back to the current version", refusal)
				}
				return
			}
			if refusal != "" {
				t.Fatalf("unexpected refusal %q", refusal)
			}
			if v != c.wantVersion {
				t.Errorf("version = %d, want %d", v, c.wantVersion)
			}
			if len(st.asked) != c.wantAsked {
				t.Errorf("the store was consulted %d times, want %d", len(st.asked), c.wantAsked)
			}
		})
	}
}

// ---- driven through executeWorkflow ------------------------------------------------------------------------------
//
// TestContinuationVersion pins the DECISION and cannot see whether the call site uses it: a mutant that computes the
// version and then passes wf.DefVersion to ContinueAsNew compiles and passes it (measured, and cleat-review's reason for
// FINAL NOT OK on the first head of this PR). These drive the real branch in executeWorkflow, with a stub backend whose
// "guest" asks the engine's own host handler to continue as new, so the suspension the worker acts on is the engine's, not
// a hand-built one. They also reach the two paths a database cannot: a refused version and a failed read.

// continuingGuest is a backend whose guest continues as new with `version` (0 = the plain form) and returns.
type continuingGuest struct{ version int }

func (g *continuingGuest) Execute(ctx context.Context, _ []byte, _ string, _ json.RawMessage, h engine.HostHandler) (*engine.ExecResult, error) {
	if g.version == 0 {
		h.ContinueAsNew(ctx, nil, `{"n":1}`)
	} else {
		h.ContinueAsNewWithVersion(ctx, nil, `{"n":1}`, g.version)
	}
	return &engine.ExecResult{}, nil
}
func (g *continuingGuest) Close(context.Context) error                   { return nil }
func (g *continuingGuest) Name() string                                  { return "continuing-guest" }
func (g *continuingGuest) PerExecution(time.Duration) engine.WasmBackend { return g }

type continuationFixture struct {
	w         *Worker
	continued []int // the defVersion each ContinueAsNew call was given
	releases  int
	failMsgs  []string
	failCodes []string
	logs      bytes.Buffer
}

func newContinuationFixture(t *testing.T, requested int, validate func(context.Context, string, int) (bool, error)) *continuationFixture {
	t.Helper()
	f := &continuationFixture{}
	ms := &mockStore{
		loadWASMFn: func(_ context.Context, _ string, _ int) ([]byte, error) {
			return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, nil
		},
		validateVersionFn: validate,
		continueAsNewFn: func(_ context.Context, _, _ string, _ int64, _ string, defVersion int, _ json.RawMessage, _ string, _ map[string]string, _ int) (string, error) {
			f.continued = append(f.continued, defVersion)
			return "next-run", nil
		},
		releaseWorkflowFn: func(context.Context, string, string, int64, time.Time) error {
			f.releases++
			return nil
		},
		failWorkflowFn: func(_ context.Context, _, _ string, _ int64, errMsg, errorCode, _ string, _ map[string]string) error {
			f.failMsgs = append(f.failMsgs, errMsg)
			f.failCodes = append(f.failCodes, errorCode)
			return nil
		},
	}
	f.w = newTestWorker(ms)
	f.w.wasmtimeBackend = &continuingGuest{version: requested}
	f.w.logger = slog.New(slog.NewTextHandler(&f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return f
}

func (f *continuationFixture) run() {
	wf := &engine.WorkflowInstance{
		ID: "run-1", DefName: "chain", DefVersion: 1, TenantID: "t-1", Generation: 3,
		Input: json.RawMessage(`{"__entry_point":"test"}`),
	}
	f.w.wg.Add(1) // executeWorkflow defers w.wg.Done()
	f.w.executeWorkflow(wf)
}

// 1. The version the guest asked for is the version the store is told to start the next run on.
func TestAWorkerStartsTheNextRunOnTheRequestedVersion(t *testing.T) {
	f := newContinuationFixture(t, 2, func(_ context.Context, _ string, v int) (bool, error) { return v == 2, nil })
	f.run()
	if len(f.continued) != 1 {
		t.Fatalf("ContinueAsNew was called %d time(s), want 1: the branch under test was not reached.\n\nLog:\n%s", len(f.continued), f.logs.String())
	}
	if f.continued[0] != 2 {
		t.Fatalf("the next run was started on version %d, want 2: the worker ignored the requested version (cleat#4041).\n\nLog:\n%s", f.continued[0], f.logs.String())
	}
	if len(f.failMsgs) != 0 || f.releases != 0 {
		t.Errorf("fails=%v releases=%d, want none: an honoured request is not a failure", f.failMsgs, f.releases)
	}
}

// The control: the plain form stays on the current version, and asks the store nothing about versions.
func TestAWorkerKeepsThePlainContinueAsNewOnTheCurrentVersion(t *testing.T) {
	asked := 0
	f := newContinuationFixture(t, 0, func(context.Context, string, int) (bool, error) { asked++; return true, nil })
	f.run()
	if len(f.continued) != 1 || f.continued[0] != 1 {
		t.Fatalf("ContinueAsNew got versions %v, want [1]: a plain continue_as_new must stay on the current version.\n\nLog:\n%s", f.continued, f.logs.String())
	}
	if asked != 0 {
		t.Errorf("the store was asked about a version %d time(s) for a plain continue_as_new, want 0", asked)
	}
}

// 2. A version that is not deployed fails the run, names the version, and never reaches ContinueAsNew.
func TestAWorkerFailsTheRunWhenTheRequestedVersionIsNotDeployed(t *testing.T) {
	f := newContinuationFixture(t, 2, func(context.Context, string, int) (bool, error) { return false, nil })
	f.run()
	if len(f.continued) != 0 {
		t.Fatalf("ContinueAsNew was called with %v for a version that is not deployed: it fell back instead of failing.\n\nLog:\n%s", f.continued, f.logs.String())
	}
	if len(f.failMsgs) != 1 {
		t.Fatalf("FailWorkflow was called %d time(s), want 1.\n\nLog:\n%s", len(f.failMsgs), f.logs.String())
	}
	for _, want := range []string{"version 2", `"chain"`, "not deployed", "rather than continued on version 1"} {
		if !strings.Contains(f.failMsgs[0], want) {
			t.Errorf("the failure %q does not say %q", f.failMsgs[0], want)
		}
	}
	if f.failCodes[0] != engine.ErrPermanent.String() {
		t.Errorf("error code %q, want %q: nothing about the next attempt would change", f.failCodes[0], engine.ErrPermanent.String())
	}
	if f.releases != 0 {
		t.Errorf("releases = %d, want 0: a refusal fails the run, it does not hand it back", f.releases)
	}
}

// 3. A failed READ of the definition says nothing about the version: the run is released, not failed, and nothing is started.
func TestAWorkerReleasesTheRunWhenTheVersionCannotBeChecked(t *testing.T) {
	f := newContinuationFixture(t, 2, func(context.Context, string, int) (bool, error) {
		return false, errors.New("validate version: driver: bad connection")
	})
	f.run()
	if len(f.continued) != 0 {
		t.Fatalf("ContinueAsNew was called with %v after a failed version check.\n\nLog:\n%s", f.continued, f.logs.String())
	}
	if len(f.failMsgs) != 0 {
		t.Fatalf("the run was FAILED (%v) over a transient read error: a read says nothing about the version.\n\nLog:\n%s", f.failMsgs, f.logs.String())
	}
	if f.releases != 1 {
		t.Fatalf("releases = %d, want 1.\n\nLog:\n%s", f.releases, f.logs.String())
	}
}
