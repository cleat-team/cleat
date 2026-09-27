package main

// cleat#1009: error_code was ALWAYS "unknown" for a workflow that died of a
// retry exhaustion, which is the one case the engine knows the answer to.
//
// The issue's original premise -- that error_code is never returned -- was
// retracted: engine/store_types.go projects it. Storage and projection both
// work; only the PRODUCER was missing. The caller derives its code with
// errors.As against *engine.CleatError, and that cannot match, because the
// engine hands its typed error to the GUEST and the guest returns a plain
// string out of its entry point.
//
// Calls the REAL rule -- classifyTerminalErrorCode, which writeTerminalFailure
// uses -- rather than a copy of it in this file. A first version mirrored the
// rule here and both sabotages below left the table green, because a mirror
// cannot notice the original changing. Written against the rule rather than
// through a booted worker for the reason the sibling cleat#1155 test gives: a
// worker exercises a dozen layers that can each independently decide the
// answer.

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestErrorCodeSaysRetriesExhausted(t *testing.T) {
	unknown := engine.ErrUnknown.String()
	permanent := engine.ErrPermanent.String()
	exhaustedCode := engine.ErrRetriesExhausted.String()

	cases := []struct {
		name         string
		errorCode    string
		deadLettered bool
		want         string
		why          string
	}{
		{
			name: "the defect", errorCode: unknown, deadLettered: true,
			want: exhaustedCode,
			why:  "the engine knows retries were exhausted; before this it stored 'unknown'",
		},
		{
			// CONTROL. Without it, `errorCode = retries_exhausted` whenever
			// dead-lettered would pass the case above and destroy a real
			// classification.
			name: "a genuine code is NOT overwritten", errorCode: permanent, deadLettered: true,
			want: permanent,
			why: "ErrPermanent from a version check is information this signal does not have; " +
				"trading it for a narrower answer would lose more than it gains",
		},
		{
			// CONTROL. The upgrade must be gated on the signal, not applied
			// to every unknown failure.
			name: "an ordinary failure stays unknown", errorCode: unknown, deadLettered: false,
			want: unknown,
			why:  "not every unknown failure is a retry exhaustion",
		},
		{
			// CONTROL for the panic path, which passes eligibleForDLQ=false so
			// deadLettered is false however the history reads. A recovered
			// panic is a crash, not a call that ran out of attempts.
			name: "the panic path keeps its own code", errorCode: unknown, deadLettered: false,
			want: unknown,
			why:  "eligibleForDLQ=false must keep the code out of the exhaustion bucket",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTerminalErrorCode(tc.errorCode, tc.deadLettered)
			if got != tc.want {
				t.Errorf("error_code = %q, want %q\n%s", got, tc.want, tc.why)
			}
		})
	}
}
