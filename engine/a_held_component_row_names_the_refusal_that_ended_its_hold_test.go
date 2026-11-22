package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4162. The held-component rows (Python, C#) have failed once in a while under load with `suspended=true ops=[Charge]` and nothing in the message says WHY the wait was not
// held: a refusal by the resident set (and then for which reason) and a wait that never reached it look identical. The set already counts every refusal by reason, so the rows
// now print that count (heldRun.refusals) in the failure message, read back through the same /metrics handler an operator scrapes.
//
// These tests are about that reporting and nothing else. They do not explain the earlier failures and are not evidence about their cause.

func TestResidentRefusalsReadsBackWhatTheCounterRecorded(t *testing.T) {
	m, err := prometheus.New(prometheus.Config{})
	if err != nil {
		t.Fatalf("prometheus.New: %v", err)
	}
	// With nothing recorded the answer is the explicit absence, not an empty string, so a failure message never ends in "refusals: ".
	if got := residentRefusals(t, m); got != "none recorded" {
		t.Errorf("nothing refused, got %q, want %q", got, "none recorded")
	}
	ctx := context.Background()
	m.RecordResidentRefusal(ctx, "sleep", "memory")
	m.RecordResidentRefusal(ctx, "sleep", "memory")
	m.RecordResidentRefusal(ctx, "signals", "kind")
	// Sorted, with counts, one entry per (kind, reason). The HELP and TYPE comment lines also contain the metric's name and must not be read as samples.
	const want = "kind=signals reason=kind x1; kind=sleep reason=memory x2"
	if got := residentRefusals(t, m); got != want {
		t.Errorf("residentRefusals = %q, want %q", got, want)
	}
}

// The end-to-end known-positive, on a real component under real wasmtime: a set that CAN refuse is made to (a memory budget below one C# instance), and the refusal reason has to
// arrive in the heldRun the rows report from. The control is the same run under the default policy, which is held and reports nothing: without it "a refusal was reported" is also
// what a reporter that always prints one looks like.
func TestAHeldComponentRowReportsTheReasonItWasRefused(t *testing.T) {
	b := csharpFixtureStamped(t)
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the stamped fixture is detected as %q, not csharp", got)
	}
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	const input = `{"mode":"sleep"}`

	held, err := tryExecuteAComponentUnder(t, b, langs, input, true, "csharp-fixture@1", nil, nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	if held.suspended || len(held.ops) != 2 {
		t.Fatalf("PRECONDITION FAILED (control): suspended=%v ops=%v, want the run held through its sleep under the default policy. Refusals: %s", held.suspended, held.ops, held.refusals)
	}
	if held.refusals != "none recorded" {
		t.Errorf("a run that was held reported refusals %q, want %q", held.refusals, "none recorded")
	}

	refused, err := tryExecuteAComponentUnder(t, b, langs, input, true, "csharp-fixture@1", nil, func(c *ResidencyConfig) { c.MemoryBudgetBytes = 1 })
	if err != nil {
		t.Fatalf("refused run: %v", err)
	}
	if !refused.suspended {
		t.Fatalf("PRECONDITION FAILED: a 1-byte memory budget did not make the wait suspend (ops %v), so there was nothing to report", refused.ops)
	}
	if !strings.Contains(refused.refusals, "kind=sleep reason=memory") {
		t.Errorf("the run was refused for its memory charge and the heldRun reports %q, want it to name kind=sleep reason=memory", refused.refusals)
	}
}
