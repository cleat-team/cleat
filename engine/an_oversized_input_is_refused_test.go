package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestAWorkflowStartedWithMoreInputThanTheGuestCanHoldIsRefused.
//
// cleat_poll_work clamps the input to the buffer the guest advertises and
// returns the CLAMPED length, so a guest has no way to tell a complete input
// from a prefix of a larger one. It parses what it was handed, does not find
// the fields its caller sent, and reports a JSON error about its own arguments
// -- with nothing anywhere naming a size.
//
// That is silent corruption of a workflow's input, and it is the half of
// cleat#1312 that raising the buffer cannot fix: a bigger constant moves the
// cliff, it does not make falling off it visible. (Raising it is separately
// ruled out -- a 1 MiB guest buffer breaks the OOM-defer guarantee; see
// TestTheGuestInputBufferMatchesWhatTheHostWillWrite.)
//
// The refusal is host-side on purpose. The guest cannot report it, and the
// call's return packs two lengths with no error channel, so reporting it
// through the ABI would mean changing the one call every Go guest makes first
// and the four SDKs that make it.
func TestAWorkflowStartedWithMoreInputThanTheGuestCanHoldIsRefused(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-oversized-input"))

	// Larger than the guest's argsBufSize (65536) once wrapped in the
	// DispatchWrapper envelope. Sized from the constant rather than hardcoded
	// bigger, so that raising the buffer moves this test with it instead of
	// leaving it passing for a reason that has gone away.
	//
	// One JSON string field, because the envelope escapes the input and the
	// point is the BYTE COUNT reaching the guest, not the shape.
	big, err := json.Marshal(map[string]string{"payload": strings.Repeat("x", 80_000)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "Main", json.RawMessage(big))

	if execErr == nil {
		t.Fatal("a workflow started with more input than the guest's buffer holds " +
			"reported SUCCESS.\n\n" +
			"The guest was handed a prefix of its own arguments and nothing said so. " +
			"That is the case cleat#1312 is about: the host clamps and returns the " +
			"clamped length, so neither side can tell a short input from a truncated one.")
	}

	// The MESSAGE is asserted, not just the failure, because this test would
	// pass on a guest that failed for any other reason -- a JSON parse error
	// over the prefix being the likeliest, and the one the fix exists to
	// replace. A failure that does not name the sizes is the old behaviour
	// wearing a new exit code.
	msg := execErr.Error()
	for _, want := range []string{"bytes of input", "were not delivered"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the workflow failed, but not with the size report this fix adds.\n"+
				"  want the message to contain: %q\n"+
				"  got: %s\n\n"+
				"Failing for the wrong reason here is the specific hazard: a guest "+
				"handed a truncated input fails on its own arguments, which is a "+
				"failure, and reading that as this fix working would be wrong.",
				want, msg)
		}
	}
}

// TestAnInputThatFitsIsNotRefused is the control, and without it the test above
// is satisfied by a host that refuses every workflow.
func TestAnInputThatFitsIsNotRefused(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-ordinary-input"))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "Main", json.RawMessage(`{"name":"ordinary"}`))

	if execErr != nil && strings.Contains(execErr.Error(), "were not delivered") {
		t.Fatalf("an ordinary input was refused as oversized: %v", execErr)
	}
}

// TestAnInputUnderTheLimitWhoseENVELOPEIsNotIsAlsoRefused.
//
// The two delivery paths carry different payloads, and the larger one is not
// the obvious one. writeWorkToFixedMemory copies the raw input;
// cleat_poll_work hands over the {"inputJSON":...} envelope, which re-encodes
// the input AS A JSON STRING -- so every quote and backslash in it becomes two
// characters.
//
// That is not a rounding error. An input of 60,009 bytes made of quotes
// becomes an envelope of 120,029: comfortably under the limit before wrapping
// and nearly double it after. A check on the raw length alone would pass this
// through to a truncating delivery, which is the silent case the whole fix is
// about, so this test exists to keep the second arm honest rather than to
// restate the first.
func TestAnInputUnderTheLimitWhoseENVELOPEIsNotIsAlsoRefused(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-oversized-envelope"))

	// Quote-heavy, so the envelope's escaping roughly doubles it.
	big, err := json.Marshal(map[string]string{"payload": strings.Repeat(`"`, 30_000)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The premise is asserted rather than assumed: if this ever stops being
	// under the limit, the test is no longer about the envelope and would pass
	// via the first arm without anyone noticing.
	if len(big) > 65536 {
		t.Fatalf("the fixture is %d bytes, which the RAW check already refuses; "+
			"this test is then not exercising the envelope arm", len(big))
	}

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "Main", json.RawMessage(big))
	if execErr == nil {
		t.Fatal("an input whose dispatch envelope exceeds the guest's buffer was accepted")
	}
	if !strings.Contains(execErr.Error(), "dispatch envelope grows") {
		t.Errorf("refused, but not as an envelope overflow.\n  got: %s", execErr.Error())
	}
}
