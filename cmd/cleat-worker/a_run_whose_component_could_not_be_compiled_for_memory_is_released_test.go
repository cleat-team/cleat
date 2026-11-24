package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// A component whose precompile child was killed (almost certainly the container's memory limit) never reached its guest, so the run did nothing wrong. The worker releases it for the refusal's own
// retry-after instead of recording a terminal failure, and says so loudly and on a metric. The engine half (the child is killed, the worker does NOT compile it itself, and a second attempt inside the
// window starts no child) is engine/a_component_compiled_in_a_child_process_test.go.
//
// Driven through executeWorkflow with the same fixture as a_run_whose_limits_cannot_be_read_is_released_test.go, and for the same reason: the release helper being right says nothing about whether
// the branch that calls it is still there.

// refusingBackend is the WASM backend the engine routes the fixture's module to; it refuses to compile, as a worker whose child was killed does.
type refusingBackend struct {
	refuse error
	calls  atomic.Int32
}

func (r *refusingBackend) Execute(context.Context, []byte, string, json.RawMessage, engine.HostHandler) (*engine.ExecResult, error) {
	r.calls.Add(1)
	return nil, r.refuse
}
func (r *refusingBackend) Close(context.Context) error                   { return nil }
func (r *refusingBackend) Name() string                                  { return "refusing-backend" }
func (r *refusingBackend) PerExecution(time.Duration) engine.WasmBackend { return r }

func newRefusalFixture(t *testing.T, refuse error) (*limitsFixture, *refusingBackend) {
	t.Helper()
	f := newLimitsFixture(t, func(context.Context) (engine.TenantSettings, error) { return engine.TenantSettings{}, nil })
	rb := &refusingBackend{refuse: refuse}
	f.w.wasmtimeBackend = rb
	return f, rb
}

func TestARunWhoseComponentCouldNotBeCompiledForMemoryIsReleasedForItsRetryAfterNotFailed(t *testing.T) {
	const retryAfter = 7 * time.Minute
	refusal := &engine.ComponentCompileRefusedError{MemoryLimitBytes: 512 << 20, ParallelCompilation: true, RetryAfter: retryAfter, Cause: errors.New("signal: killed")}
	f, rb := newRefusalFixture(t, refusal)

	before := time.Now()
	f.run()

	if rb.calls.Load() != 1 {
		t.Fatalf("the backend was reached %d time(s), want 1: the test never got a refusal to release, so it proves nothing\n%s", rb.calls.Load(), f.logs.String())
	}
	if f.fails.Load() != 0 {
		t.Fatalf("FailWorkflow was called %d time(s). The guest never ran, so the run did nothing wrong, and a terminal failure destroys it. Log:\n%s", f.fails.Load(), f.logs.String())
	}
	if f.releases.Load() != 1 {
		t.Fatalf("releases = %d, want 1.\nLog:\n%s", f.releases.Load(), f.logs.String())
	}
	// Released for the refusal's retry-after, not the 5 s default: otherwise it is claimed straight back and refused again, every 5 s, for as long as the window lasts.
	if f.nextWake.Before(before.Add(retryAfter - time.Second)) {
		t.Errorf("released for %v, want about %v: a release shorter than the refusal window is claimed back and refused again", f.nextWake.Sub(before), retryAfter)
	}
	log := f.logs.String()
	for _, want := range []string{"for want of memory", "level=ERROR", "retry_after=7m0s", "512 MiB memory limit", "--component-precompile-parallel=false"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}

	// The metric names the check, the memory limit and the compile mode.
	rec := httptest.NewRecorder()
	f.w.Metrics.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var line string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(l, "cleat_workflow_releases_total") && strings.Contains(l, `check="component_compile_refused"`) {
			line = l
		}
	}
	for _, want := range []string{`memory_limit_mib="512"`, `parallel_compilation="true"`, `reason="child_killed"`} {
		if !strings.Contains(line, want) {
			t.Errorf("cleat_workflow_releases_total{check=\"component_compile_refused\"} = %q, want it to carry %s", line, want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(line), " 1") {
		t.Errorf("the counter line %q does not end in a count of 1", line)
	}
}

// The control, without which the test above is satisfied by a worker that releases every failed run: the same fixture with an ordinary guest failure fails the run.
func TestARunWhoseBackendFailsForAnyOtherReasonIsStillFailed(t *testing.T) {
	f, rb := newRefusalFixture(t, fmt.Errorf("a different failure"))
	f.run()

	if rb.calls.Load() != 1 {
		t.Fatalf("the backend was reached %d time(s), want 1", rb.calls.Load())
	}
	if f.fails.Load() != 1 || f.releases.Load() != 0 {
		t.Fatalf("fails=%d releases=%d, want 1 and 0: only a refused compile is released.\n%s", f.fails.Load(), f.releases.Load(), f.logs.String())
	}
}
