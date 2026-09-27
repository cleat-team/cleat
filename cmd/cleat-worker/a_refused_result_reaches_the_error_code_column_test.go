package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestARefusedResultReachesTheErrorCodeColumn is the consumer half of
// cleat#1460, and it exists because the producer half can be entirely correct
// and still deliver nothing.
//
// The store now returns a *engine.CleatError carrying ErrResultRejected. Two
// things in this file stand between that and the error_code an operator reads,
// and neither is obvious from the engine side:
//
//  1. isConnectionError is consulted FIRST, and it matches on error TEXT. A
//     message that happens to contain one of its substrings sends the workflow
//     down releaseWorkflow instead of recordTerminalFailure -- so the run is
//     retried rather than recorded, forever, and no error_code is written at
//     all. That is a worse outcome than the driver text cleat#1460 complains
//     about, and it would be caused by the wording of a message.
//  2. the code is derived with errors.As against *engine.CleatError, defaulting
//     to ErrUnknown. A classification that does not survive the wrap arrives as
//     "unknown", which is the exact symptom being fixed.
//
// The driver strings below are the ones MEASURED against live backends while
// building the fix, not invented, because the risk in (1) is entirely about
// what real driver text contains.
func TestARefusedResultReachesTheErrorCodeColumn(t *testing.T) {
	measured := []struct{ dialect, driverText string }{
		{"postgres", "pq: unsupported Unicode escape sequence (22P05)"},
		{"postgres", "pq: invalid input syntax for type json (22P02)"},
		{"mysql", `Error 3141 (22032): Invalid JSON text in argument 1 to function cast_as_json: "The surrogate pair in string is invalid." at position 6.`},
		{"mysql", "Error 3157 (22032): The JSON document exceeds the maximum depth."},
		{"mssql", "mssql: JSON text that has more than 128 nesting levels cannot be parsed."},
	}

	for _, m := range measured {
		wrapped := &engine.CleatError{
			Code:       engine.ErrResultRejected,
			Op:         "finalize workflow",
			WorkflowID: "wf-1460",
			// THE REAL MESSAGE, from the producer. Reconstructing it here would
			// test a copy: a wording change in engine that introduced one of
			// isConnectionError's substrings would leave this green.
			Err: errors.New(engine.ResultRejectionMessage(`{"v":"ok"}`) + ": " + m.driverText),
		}

		if isConnectionError(wrapped) {
			t.Errorf("%s: a refused result is read as a CONNECTION error.\n  %v\n\n"+
				"isConnectionError matches substrings of the message, so the workflow "+
				"would be RELEASED and retried rather than recorded as failed -- "+
				"no error_code is written at all, and the run retries forever. "+
				"Check the fix's wording against isConnectionError's pattern list.",
				m.dialect, wrapped)
		}

		// The derivation cmd/cleat-worker performs after a finalize failure.
		var ce *engine.CleatError
		code := engine.ErrUnknown.String()
		if errors.As(error(wrapped), &ce) {
			code = ce.Code.String()
		}
		if code != "result_rejected_by_store" {
			t.Errorf("%s: error_code would be stored as %q, not %q",
				m.dialect, code, "result_rejected_by_store")
		}
	}

	// CONTROL for isConnectionError itself. Without it, the assertions above are
	// satisfied by an isConnectionError that returns false for everything -- and
	// then this test would go green on a build where DB-down handling is broken,
	// having "verified" nothing.
	if !isConnectionError(errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")) {
		t.Fatal("isConnectionError does not recognise a real connection failure, so the " +
			"assertions above distinguish nothing")
	}

	// CONTROL for the derivation: an unclassified error must still come out
	// "unknown", or the check above passes for every input.
	var ce *engine.CleatError
	code := engine.ErrUnknown.String()
	if errors.As(errors.New("finalize workflow: something else entirely"), &ce) {
		code = ce.Code.String()
	}
	if code != "unknown" {
		t.Fatalf("an unclassified finalize error derived %q rather than \"unknown\"", code)
	}

	// And the message an operator sees must carry the fact that changes what
	// they do next.
	if !strings.Contains(engine.ResultRejectionMessage(`{"v":"ok"}`), "ALREADY RAN") {
		t.Error("the operator-facing message no longer says the workflow body already " +
			"ran. That is the fact which changes what an operator does next, and the " +
			"driver text never carried it.")
	}

	// Every pattern, against the real message, for both branches of the hint --
	// a portable result and one carrying a known non-portable escape.
	for _, r := range []string{`{"v":"ok"}`, `{"v":"\u0000"}`} {
		if isConnectionError(errors.New(engine.ResultRejectionMessage(r))) {
			t.Errorf("the real message for result %s is read as a connection error:\n  %s",
				r, engine.ResultRejectionMessage(r))
		}
	}
}
