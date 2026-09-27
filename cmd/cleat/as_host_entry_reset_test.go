package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every host entry point in a generated AssemblyScript module must clear the
// suspension flag on the way in. IMPROVEMENT-PLAN 3.106.
//
// `_workflowSuspended` (packages/cleat-as/assembly/memory.ts) means "the thing
// currently running asked to suspend". This SDK has no exceptions, so that
// flag is the ONLY signal there is -- Go panics, Java throws, Rust returns an
// Err, and each of those unwinds a stack that carries the fact away with it.
// A flag does not unwind. It survives the return to the host and is still set
// when the host calls back in.
//
// That is not hypothetical. Measured 2026-09-03 on a defer segment, before the
// fix this test guards:
//
//	defers_run=1     operations: [second]        want [second first]
//
// The body's refused call set the flag, the wrapper correctly returned
// SUSPEND_SENTINEL without draining, and the host then called
// `__cleat_run_deferred` -- which did not reset. `runDeferred` checks the flag
// after each body to stop the drain when a defer body itself suspends, read
// the flag still set from the BODY's call, and stopped after the first one.
// One cleanup ran; the other was taken off the table and silently dropped.
// Two defers, one performed, no error anywhere.
//
// The workflow wrappers always had the reset (Step 3). The defer runner is the
// one that did not, and it is the entry point that is only ever called after
// something else has already set the flag -- so it is both the easiest to
// forget and the only one where forgetting is guaranteed to bite.
//
// This reads the generator rather than a built module because it costs
// nothing: no Node, no toolchain, so it runs in every job. The behavioural
// proof is engine/as_defer_segment_e2e_test.go, which needs both.
func TestEveryGeneratedASHostEntryPointResetsTheSuspendFlag(t *testing.T) {
	const src = "../../packages/cleat-as/transform/index.js"

	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"This test reads the AssemblyScript transformer to check what it "+
			"emits. If it moved, re-point it; do not delete it.", src, err)
	}
	gen := string(b)

	// The generator writes its output with `code += \`...\`` lines, so the
	// emitted text is what these look for -- not the generator's own calls.
	entries := []struct {
		name  string
		emits string
	}{
		{"__cleat_run_deferred", "export function __cleat_run_deferred(): i64"},
		{"workflow wrapper", "resetWorkflowSuspended();"},
	}
	for _, e := range entries {
		if !strings.Contains(gen, e.emits) {
			t.Fatalf("%s does not emit %q.\n\n"+
				"The generator changed shape and this test can no longer see "+
				"what it is checking -- which is worse than a failure, because "+
				"the checks below would pass vacuously.", src, e.emits)
		}
	}

	// The defer runner's emitted body, from its `export function` line to the
	// closing brace, must contain the reset AND must contain it before the
	// drain. Order matters: resetting after runDeferred restores the bug in a
	// form that still contains the word.
	runner := regexp.MustCompile(
		`(?s)export function __cleat_run_deferred\(\): i64.*?runDeferred\(`)
	m := runner.FindString(gen)
	if m == "" {
		t.Fatalf("could not find the emitted __cleat_run_deferred body in %s", src)
	}
	if !strings.Contains(m, "resetWorkflowSuspended()") {
		t.Fatalf("the generated __cleat_run_deferred does not call "+
			"resetWorkflowSuspended() before runDeferred().\n\n"+
			"The host calls this export after a segment that has ALREADY set the "+
			"flag -- a defer segment sets it to stop the body's call, and a "+
			"killed workflow may have set it before it was killed. runDeferred "+
			"reads the flag after each body to decide whether to keep going, so "+
			"a stale one stops the drain after the FIRST defer and drops the "+
			"rest with no error. Measured before the fix: defers_run=1 on a "+
			"two-defer fixture.\n\nEmitted body:\n%s", m)
	}
}
