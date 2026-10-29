package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3565, the worker half. An execution that could not read the limits it
// must run under never ran its guest, so there is no outcome to record: the run
// is released with a backoff, never failed. The engine half (it refuses to run
// under the operator's flags, which may be wider than a tenant's own limit) is
// in engine/a_failed_limits_read_fails_closed_test.go.
//
// Driven through executeWorkflow rather than by calling the release helper: the
// helper being right says nothing about whether the branch that calls it is
// still there, which is the lesson a_stopped_run_is_released_not_failed_test.go
// records.

// guestStub is the WASM backend the engine routes the fixture's module to. Its
// guest "fails", so a run that reaches it is a genuine execution failure, and
// the count says whether the guest ran at all.
type guestStub struct{ ran atomic.Int32 }

func (g *guestStub) Execute(context.Context, []byte, string, json.RawMessage, engine.HostHandler) (*engine.ExecResult, error) {
	g.ran.Add(1)
	return nil, errors.New("stub guest failed")
}
func (g *guestStub) Close(context.Context) error                   { return nil }
func (g *guestStub) Name() string                                  { return "guest-stub" }
func (g *guestStub) PerExecution(time.Duration) engine.WasmBackend { return g }

type limitsFixture struct {
	guest     guestStub
	w         *Worker
	releases  atomic.Int32
	fails     atomic.Int32
	nextWake  time.Time
	logs      bytes.Buffer
	settingsN atomic.Int32
}

func newLimitsFixture(t *testing.T, settings func(context.Context) (engine.TenantSettings, error)) *limitsFixture {
	t.Helper()
	f := &limitsFixture{}
	ms := &mockStore{
		loadWASMFn: func(_ context.Context, _ string, _ int) ([]byte, error) {
			return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, nil
		},
		releaseWorkflowFn: func(_ context.Context, _ string, _ string, _ int64, next time.Time) error {
			f.releases.Add(1)
			f.nextWake = next
			return nil
		},
		failWorkflowFn: func(context.Context, string, string, int64, string, string, string, map[string]string) error {
			f.fails.Add(1)
			return nil
		},
		getTenantSettingsFn: func(ctx context.Context) (engine.TenantSettings, error) {
			f.settingsN.Add(1)
			return settings(ctx)
		},
	}
	f.w = newTestWorker(ms)
	// Without a backend the engine refuses the module at routing, before it
	// reads any limits, and a test of the limits read would be passing on that.
	f.w.wasmtimeBackend = &f.guest
	f.w.logger = slog.New(slog.NewTextHandler(&f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return f
}

func (f *limitsFixture) run() {
	wf := &engine.WorkflowInstance{
		ID: "run-1", DefName: "test-def", DefVersion: 1, TenantID: "t-1", Generation: 3,
		Input: json.RawMessage(`{"__entry_point":"test"}`),
	}
	f.w.wg.Add(1) // executeWorkflow defers w.wg.Done()
	f.w.executeWorkflow(wf)
}

func TestARunWhoseLimitsCannotBeReadIsReleasedNotFailed(t *testing.T) {
	f := newLimitsFixture(t, func(context.Context) (engine.TenantSettings, error) {
		return engine.TenantSettings{}, errors.New("get tenant settings: driver: bad connection")
	})

	before := time.Now()
	f.run()

	if f.settingsN.Load() == 0 {
		t.Fatal("the settings were never read, so this test proved nothing about a failed read")
	}
	if n := f.guest.ran.Load(); n != 0 {
		t.Fatalf("the guest ran %d time(s) with no readable limits", n)
	}
	if n := f.fails.Load(); n != 0 {
		t.Fatalf("FailWorkflow was called %d time(s).\n\nThe guest never ran, so the run did "+
			"nothing wrong; recording a terminal failure destroys it. Log:\n%s", n, f.logs.String())
	}
	if n := f.releases.Load(); n != 1 {
		t.Fatalf("releases = %d, want 1.\n\nLog:\n%s", n, f.logs.String())
	}
	// The backoff is what keeps a database that stays down from becoming a spin.
	if f.nextWake.Before(before.Add(defaultUnservableBackoff - time.Second)) {
		t.Errorf("released for %v, want about %v from now: a release with no backoff is claimed straight back",
			f.nextWake.Sub(before), defaultUnservableBackoff)
	}
	if !strings.Contains(f.logs.String(), "could not read this workflow's limits") {
		t.Errorf("the release did not come from the limits branch.\n\nLog:\n%s", f.logs.String())
	}
}

// The control, without which the test above is satisfied by a worker that
// releases every failed run: with a healthy settings read the same fixture
// fails the run, because its module cannot execute.
func TestARunWhoseLimitsReadFineIsStillFailedWhenItFails(t *testing.T) {
	f := newLimitsFixture(t, func(context.Context) (engine.TenantSettings, error) {
		return engine.TenantSettings{}, nil
	})

	f.run()

	if f.guest.ran.Load() != 1 {
		t.Fatalf("the guest ran %d time(s), want 1: the control never reached the guest, so it "+
			"proves nothing.\n\nLog:\n%s", f.guest.ran.Load(), f.logs.String())
	}
	if f.fails.Load() != 1 || f.releases.Load() != 0 {
		t.Fatalf("fails=%d releases=%d, want 1 and 0: with readable limits an execution that fails "+
			"is a failure.\n\nLog:\n%s", f.fails.Load(), f.releases.Load(), f.logs.String())
	}
}
