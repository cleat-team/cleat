//go:build cgo

package csharp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// The conformance harness for the C# SDK (cleat#4080, PR 5.0). Every host-call group adds entry points to
// packages/cleat-csharp/conformance/ in its own Group<X>.cs and drives them from its own tests/csharp/group_<x>_test.go, through
// runConformanceEntry below. This file is the shared skeleton and holds no group's cases.
//
// WHAT IT DOES. It builds the conformance project ONCE per test process with the real `cleat build --target csharp` (the pinned
// container, the package lock, the stamp), reads the stamped component back, and runs a named entry point through the real engine and
// the real wasmtime backend, with a recording ServiceCaller standing in for the services a workflow calls. Nothing is mocked below the
// service boundary: the guest, the generated bindings, the SDK, and the host functions are all real.
//
// WHY A BUILD TAG. NewWasmtimeBackend is cgo-only; without cgo it is a stub that errors, and a test that ran under it would measure
// nothing (CLAUDE.md, "Build and test with CGO on"). The drift test beside this file has no tag and runs everywhere.
//
// SKIPPING. needDocker and needPlatform (csharp_build_test.go) skip, or FAIL when CLEAT_REQUIRE_TOOLCHAINS declares csharp or
// csharp-<arch>, so a job that promises the toolchain cannot go green without running a single case.

type recordedCall struct{ Service, Operation, Request string }

// recordingCaller answers a service call from Responses (keyed "service/operation"), else with {}, and records every call in order.
type recordingCaller struct {
	mu        sync.Mutex
	calls     []recordedCall
	counts    map[string]int // calls so far per "service/operation", this one included
	responses map[string]string
	fail      func(service, operation string, nth int) error
}

func (c *recordingCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, recordedCall{service, operation, requestJSON})
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[service+"/"+operation]++
	// A failure is still a CALL: it is recorded above, so a test asserts both that the guest reached the service and what became of it.
	if c.fail != nil {
		if err := c.fail(service, operation, c.counts[service+"/"+operation]); err != nil {
			return "", err
		}
	}
	if r, ok := c.responses[service+"/"+operation]; ok {
		return r, nil
	}
	return "{}", nil
}

func (c *recordingCaller) recorded() []recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedCall(nil), c.calls...)
}

var (
	conformanceOnce sync.Once
	conformanceWasm []byte
	conformanceErr  string // non-empty means the build did not produce a component: every case fails with it, not skips
)

// conformanceComponent builds the conformance project once and returns the stamped component.
func conformanceComponent(t *testing.T) []byte {
	t.Helper()
	needDocker(t)
	root := repoRoot(t)
	needPlatform(t, root, "linux/"+runtime.GOARCH)
	conformanceOnce.Do(func() {
		work, err := os.MkdirTemp("", "cleat-conformance-")
		if err != nil {
			conformanceErr = fmt.Sprintf("no temporary directory: %v", err)
			return
		}
		defer os.RemoveAll(work)
		bin := filepath.Join(work, "cleat")
		build := exec.Command("go", "build", "-o", bin, "./cmd/cleat")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			conformanceErr = fmt.Sprintf("building cleat: %v\n%s", err, out)
			return
		}
		outDir := filepath.Join(work, "out")
		cmd := exec.Command(bin, "build", "--target", "csharp", "-o", outDir, "--version", "1", "packages/cleat-csharp/conformance")
		cmd.Dir = root
		log, err := cmd.CombinedOutput()
		if err != nil {
			code := -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			// 1 is a finding about the project and 2 is UNMEASURED (the toolchain or its pins failed): both fail the cases, and the log
			// says which, because the two send the reader to different places.
			conformanceErr = fmt.Sprintf("cleat build --target csharp exited %d (1 = a finding about the project, 2 = UNMEASURED):\n%s", code, log)
			return
		}
		b, err := os.ReadFile(filepath.Join(outDir, "conformance.wasm"))
		if err != nil {
			conformanceErr = fmt.Sprintf("the build succeeded but left no conformance.wasm: %v\n%s", err, log)
			return
		}
		conformanceWasm = b
	})
	if conformanceErr != "" {
		t.Fatalf("the conformance component was not built, so nothing below was measured:\n%s", conformanceErr)
	}
	return conformanceWasm
}

type entryRun struct {
	WorkflowID string
	Input      string            // JSON; "{}" when empty
	Responses  map[string]string // "service/operation" -> response JSON, for the recording caller
	// Fail makes a service call fail: it is asked for every call with the 1-based count of calls to that service/operation so far, and a
	// non-nil error is what the service "returns". A failed call is still recorded in Calls. Nil means every call succeeds.
	Fail    func(service, operation string, nth int) error
	Options []engine.EngineOption // stores and the like; a group that needs a store passes it here
	Replay  []engine.EventRecord  // when non-nil the entry is REPLAYED over this history instead of executed fresh
}

type conformanceResult struct {
	Result  string
	History []engine.EventRecord
	// QueryState is what the guest set with set_query_state, as the engine reports it at the end of the run.
	QueryState map[string]string
	Suspended  *engine.SuspendResult
	Calls      []recordedCall
	Err        error
}

// runConformanceEntry runs one entry point of the conformance component through the real engine and wasmtime.
func runConformanceEntry(t *testing.T, entry string, r entryRun) conformanceResult {
	t.Helper()
	b := conformanceComponent(t)
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the component is detected as %q, not csharp, so a different backend path would be measured", got)
	}
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &recordingCaller{responses: r.Responses, fail: r.Fail}
	id := r.WorkflowID
	if id == "" {
		id = "wf-conformance-" + strings.ReplaceAll(t.Name(), "/", "-")
	}
	input := r.Input
	if input == "" {
		input = "{}"
	}
	opts := append([]engine.EngineOption{engine.WithBackends(engine.WasmtimeLanguages, wt), engine.WithWorkflowID(id)}, r.Options...)
	eng := engine.NewEngine(rt, caller, opts...)
	var (
		res   string
		hist  []engine.EventRecord
		susp  *engine.SuspendResult
		state map[string]string
	)
	if r.Replay != nil {
		res, hist, susp, _, state, err = eng.Replay(ctx, b, entry, json.RawMessage(input), r.Replay)
	} else {
		res, hist, susp, _, state, err = eng.Execute(ctx, b, entry, json.RawMessage(input))
	}
	return conformanceResult{Result: res, History: hist, QueryState: state, Suspended: susp, Calls: caller.recorded(), Err: err}
}

// TestCSharpConformanceSmoke is the foundation's own case: the build, the stamp, the engine, the SDK's entry wrapper and one host call that
// the engine answers itself, with nothing group-specific. If this is red, no group's result means anything.
func TestCSharpConformanceSmoke(t *testing.T) {
	res := runConformanceEntry(t, "smoke", entryRun{WorkflowID: "wf-smoke", Input: `{"x":1}`})
	if res.Err != nil {
		t.Fatalf("the smoke entry failed: %v", res.Err)
	}
	if res.Suspended != nil {
		t.Fatalf("the smoke entry suspended (%v); it makes no call that can", res.Suspended.Reason)
	}
	var got struct {
		ID   string          `json:"id"`
		Echo json.RawMessage `json:"echo"`
	}
	if err := json.Unmarshal([]byte(res.Result), &got); err != nil {
		t.Fatalf("the result is not the JSON object the entry returns: %v\n%s", err, res.Result)
	}
	if got.ID != "wf-smoke" {
		t.Errorf("id = %q, want %q: the workflow id the engine was given did not reach the guest", got.ID, "wf-smoke")
	}
	if string(got.Echo) != `{"x":1}` {
		t.Errorf("echo = %s, want the input back", got.Echo)
	}
	if len(res.Calls) != 0 {
		t.Errorf("the smoke entry reached the service caller %d time(s): %v", len(res.Calls), res.Calls)
	}
}

// TestCSharpConformanceReportsAnEntryThatDoesNotExist is the harness's own negative control: it must be able to SEE a failure. A harness that
// reported success for a name no guest exports would make every group's "it ran" mean nothing.
func TestCSharpConformanceReportsAnEntryThatDoesNotExist(t *testing.T) {
	res := runConformanceEntry(t, "no_such_entry", entryRun{})
	if res.Err == nil {
		t.Fatalf("an entry the component does not define was reported as a success: result %q suspended=%v", res.Result, res.Suspended != nil)
	}
	if !strings.Contains(res.Err.Error(), "no_such_entry") {
		t.Errorf("the error does not name the missing entry, so it could be about anything: %v", res.Err)
	}
	t.Logf("an unknown entry is reported as: err=%v suspended=%v result=%q", res.Err, res.Suspended != nil, res.Result)
}
