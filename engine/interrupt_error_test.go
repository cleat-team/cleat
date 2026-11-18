//go:build cgo

package engine

// cleat#3922: only a trap the HOST asked for is reported as the kill it was enforcing.
//
// The soft kill's surface is the plugin runtime's own abort path, which wasmtime renders as a bare
// `unreachable` -- indistinguishable from a guest that executes `unreachable` itself. So the
// mapping is gated on the host's own record that it raised the interrupt, and this file pins both
// sides of that gate: unmapped when nobody asked, mapped and correctly named when the host did.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// genuineTrap is what the backend sees from a guest that raised `unreachable` on its own.
var genuineTrap = errors.New("wasm trap: wasm `unreachable` instruction executed")

func TestAGenuineUnreachableTrapIsNotReportedAsAKill(t *testing.T) {
	b := &wasmtimeBackend{limits: wasmtimeLimits{instructionLimit: 400_000_000}}
	if mapped := b.interruptError(genuineTrap, time.Second); mapped != nil {
		t.Fatalf("a genuine `unreachable` was mapped to %v.\n\n"+
			"The host did not raise the interrupt, so the guest crashed on its own; reporting that as an "+
			"instruction or time limit sends whoever debugs it at the wrong subsystem.", mapped)
	}
	// And the ordinary classifier must leave it alone too.
	if mapped := b.resourceLimitError(genuineTrap, time.Second); mapped != nil {
		t.Fatalf("resourceLimitError classified a bare `unreachable` as %v", mapped)
	}
}

func TestAnInterruptTheHostRaisedIsReportedAsTheKillItEnforced(t *testing.T) {
	cases := []struct {
		name string
		arm  int
		want string
	}{
		{"fuel arm", interruptArmFuel, "instruction limit exceeded (400000000 fuel units"},
		{"epoch arm", interruptArmTime, "execution time limit exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &wasmtimeBackend{
				limits:             wasmtimeLimits{instructionLimit: 400_000_000},
				interruptRequested: true,
				interruptArm:       tc.arm,
			}
			mapped := b.interruptError(genuineTrap, 30*time.Second)
			if mapped == nil {
				t.Fatal("the host raised the interrupt and the trap was not mapped: the user would see a bare `unreachable`")
			}
			if !strings.Contains(mapped.Error(), tc.want) {
				t.Errorf("mapped to %q, want it to name %q -- the arm that fired decides which bound the user is told about", mapped, tc.want)
			}
			var limit *executionLimitError
			if !errors.As(mapped, &limit) {
				t.Errorf("mapped to %T, want *executionLimitError so the worker classifies it as a resource kill", mapped)
			}
		})
	}
}

// TestTheUnwindGrantIsGivenOncePerExecution: a second poll, or a second arm firing, must not grant
// again. Otherwise a guest near its budget could keep polling its way past the bound the reserve
// exists to respect.
func TestTheUnwindGrantIsGivenOncePerExecution(t *testing.T) {
	b := &wasmtimeBackend{budget: &hostBudget{remaining: 50 * time.Millisecond}}
	b.grantUnwind()
	if got := b.budget.remaining; got != 50*time.Millisecond+interruptUnwindGrant {
		t.Fatalf("after one grant the budget is %s, want %s", got, 50*time.Millisecond+interruptUnwindGrant)
	}
	b.grantUnwind()
	b.grantUnwind()
	if got := b.budget.remaining; got != 50*time.Millisecond+interruptUnwindGrant {
		t.Fatalf("the grant was given again: the budget is now %s, want it unchanged at %s", got, 50*time.Millisecond+interruptUnwindGrant)
	}
}
