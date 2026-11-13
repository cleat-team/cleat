//go:build cgo

package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// cleat#3746. A merge-group run failed TestPluginCalls_Wasm_Python's replay with
//
//	execution time limit exceeded (1ms wall-clock budget; configure with --wasm-instance-timeout)
//
// for a 19 MB component. The 1ms was not configured by anyone: configureStore
// clamped a ctx whose deadline had ALREADY PASSED to one millisecond, ran the
// guest for one 50ms epoch tick, and reported the resulting trap as the
// instance timeout. What had spent the deadline was time before the guest
// started (the native component path compiles first and caches nothing).
//
// These tests pin three things:
//
//  1. a deadline already past refuses to start the guest, with an error that
//     wraps context.DeadlineExceeded, and the executor reports it as it reports
//     every other workflow-duration expiry ("execution timed out");
//  2. when the deadline is what bounds the guest, the error names it and does
//     NOT send the operator to --wasm-instance-timeout;
//  3. the fence that runs DURING execution is unchanged: a deadline that
//     passes while the guest runs still interrupts it.

// tinySpinComponent is a 157-byte component whose core module's start function
// loops for ever, so instantiating it is what the epoch fence has to stop.
// Built with `wasm-tools parse` and `wasm-tools component new` from
//
//	(module (func $s (loop $l br $l)) (start $s))
//
// and embedded so the test needs no toolchain.
const tinySpinComponentHex = "0061736d0d00010001350061736d01000000010401600000030201000801000a0901070003400c000b0b0013046e616d65010401000173030601000100016c02040100000000250e636f6d706f6e656e742d6e616d65010900110100046d61696e010900120100046d61696e002f0970726f647563657273010c70726f6365737365642d6279010d7769742d636f6d706f6e656e7407302e3236312e30"

func spinComponent(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(tinySpinComponentHex)
	if err != nil {
		t.Fatalf("embedded component: %v", err)
	}
	return b
}

func newDeadlineTestBackend(t *testing.T) *wasmtimeBackend {
	t.Helper()
	ctx := context.Background()
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = wt.Close(ctx) })
	return wt.PerExecution(0).(*wasmtimeBackend)
}

// A deadline that has passed refuses to start the guest.
func TestConfigureStoreRefusesAnAlreadySpentDeadline(t *testing.T) {
	b := newDeadlineTestBackend(t)
	store := wasmtime.NewStore(b.engine)
	defer store.Close()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	timeout, err := b.configureStore(ctx, store)
	if err == nil {
		t.Fatalf("configureStore accepted a ctx whose deadline passed a second ago and applied a %v budget; "+
			"the guest would run for a tick and be reported as an instance-timeout trap", timeout)
	}
	if !errors.Is(err, errDeadlineBeforeGuest) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v must be errDeadlineBeforeGuest and wrap context.DeadlineExceeded", err)
	}
	if isExecutionLimit(err) {
		t.Errorf("a spent deadline is not a guest resource limit, but %v is classified as one", err)
	}
}

// The native component path, end to end: an expired ctx must produce the
// deadline error, never a 1ms trap. This is the shape of the reported failure,
// minus the 19 MB.
func TestComponentPathWithASpentDeadlineIsNotAOneMillisecondTrap(t *testing.T) {
	b := newDeadlineTestBackend(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := b.ExecuteComponentCGo(ctx, spinComponent(t), "run", []byte(`{}`), OutBufSize)
	if err == nil {
		t.Fatal("ExecuteComponentCGo succeeded under a spent deadline")
	}
	if strings.Contains(err.Error(), "wall-clock budget") {
		t.Errorf("a spent deadline was reported as a wall-clock budget trap: %v", err)
	}
	if !errors.Is(err, errDeadlineBeforeGuest) {
		t.Errorf("error %v does not report that the deadline passed before the guest started", err)
	}
}

// The fence DURING execution is unchanged, and when the deadline is what stopped
// the guest the error says so.
func TestComponentFenceStillInterruptsAGuestThatOutlivesTheDeadline(t *testing.T) {
	b := newDeadlineTestBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := b.ExecuteComponentCGo(ctx, spinComponent(t), "run", []byte(`{}`), OutBufSize)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a guest that never returns completed")
	}
	if !isExecutionLimit(err) {
		t.Fatalf("a deadline that passes during execution must still be an execution limit: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "the time left on the workflow's deadline") {
		t.Errorf("the deadline was the bound, and the error does not say so: %v", err)
	}
	if strings.Contains(msg, "--wasm-instance-timeout") {
		t.Errorf("the guest was inside --wasm-instance-timeout (30s); naming it misdirects: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the fence took %v to stop a guest with a 500ms deadline", elapsed)
	}
}

// Which flag the message names follows which bound applied.
func TestTimeLimitErrorNamesTheBoundThatApplied(t *testing.T) {
	trap := fmt.Errorf("component instantiate: %s", "wasm trap: interrupt")
	for _, tc := range []struct {
		name        string
		ctxTimeout  time.Duration // 0 = no ctx deadline
		wantDeadl   bool
		wantInstTmo bool
	}{
		{"no ctx deadline: the instance timeout applies", 0, false, true},
		{"deadline tighter than the instance timeout: the deadline applies", 10 * time.Second, true, false},
		{"deadline looser than the instance timeout: the instance timeout applies", time.Hour, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newDeadlineTestBackend(t)
			store := wasmtime.NewStore(b.engine)
			defer store.Close()
			ctx := context.Background()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTimeout)
				defer cancel()
			}
			timeout, err := b.configureStore(ctx, store)
			if err != nil {
				t.Fatalf("configureStore: %v", err)
			}
			got := b.resourceLimitError(trap, timeout)
			if got == nil {
				t.Fatal("an epoch interrupt was not classified as a limit")
			}
			msg := got.Error()
			if hasDeadl := strings.Contains(msg, "the time left on the workflow's deadline"); hasDeadl != tc.wantDeadl {
				t.Errorf("names the deadline = %v, want %v: %s", hasDeadl, tc.wantDeadl, msg)
			}
			if hasFlag := strings.Contains(msg, "--wasm-instance-timeout"); hasFlag != tc.wantInstTmo {
				t.Errorf("names --wasm-instance-timeout = %v, want %v: %s", hasFlag, tc.wantInstTmo, msg)
			}
			// Callers that parse the number rely on this prefix being the same either way.
			if !strings.Contains(msg, "execution time limit exceeded (") || !strings.Contains(msg, " wall-clock budget") {
				t.Errorf("the common wording changed: %s", msg)
			}
		})
	}
}

// The executor reports a deadline that passed before the guest started as every
// other workflow-duration expiry, not as a guest that failed.
func TestExecutorReportsASpentDeadlineAsATimeout(t *testing.T) {
	for _, tc := range []struct {
		name        string
		backendErr  error
		wantTimeout bool
	}{
		{"deadline passed before the guest started", fmt.Errorf("component call: %w", errDeadlineBeforeGuest), true},
		// The control: the mapping is for THIS error, not for every error under an expired ctx.
		{"any other backend error", errors.New("something else broke"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &configurableMockBackend{
				executeFn: func(context.Context, []byte, string, json.RawMessage, HostHandler) (*ExecResult, error) {
					return nil, tc.backendErr
				},
			}
			e := NewEngine(nil, nil, WithBackend("go", backend), WithDefaultWorkflowTimeout(time.Nanosecond))
			e.workflowID = "wf-spent-deadline"
			e.defName = "test-def"

			_, _, _, _, _, err := e.executeWithBackend(context.Background(), backend, minimalWasm(), "test_entry", []byte(`{}`), nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			isTimeout := strings.Contains(err.Error(), "execution timed out")
			if isTimeout != tc.wantTimeout {
				t.Errorf("reported as timed out = %v, want %v: %v", isTimeout, tc.wantTimeout, err)
			}
			if tc.wantTimeout && strings.Contains(err.Error(), "execution failed") {
				t.Errorf("a workflow-duration expiry was reported as the guest failing: %v", err)
			}
		})
	}
}
