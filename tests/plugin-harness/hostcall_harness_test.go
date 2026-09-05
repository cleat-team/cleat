package pluginharness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// The host-call execution harness.
//
// scripts/sdk-host-call-coverage.py measures whether an SDK fixture *calls* a
// host method -- a compile-time question, answered by parsing source. This
// answers the other one: does the call, when a guest actually makes it against
// a host, come back with what the guest expects? Those are different questions
// and the second is where every binding defect of 2026-09-04/05 lived. All six
// of them compile.
//
// Wave 1 is the 23 result-carrying host calls: the adapters that decode
// something a guest must then interpret -- an out buffer, a packed length, a
// host message. Re-derive the split from wasm.AdapterFieldNames(), not by
// grepping wasm/adapter_metadata.go: the struct literal defeats the obvious
// regex and returns zero entries, which silently confirms any claim made about
// the remainder.

// wave1Calls is the list the fixtures are driven from AND the list the
// assertions run over. One list, deliberately: a fixture that stopped
// exercising a call while the assertion list still named it would otherwise
// pass by never being asked.
var wave1Calls = []string{
	"AcquireLock",
	"AwaitAllChildren", "AwaitAnyChild", "AwaitChild", "AwaitPromise",
	"ChildWorkflow", "ChildWorkflowWithOptions", "CreatePromise",
	"DurableAwaitSignals", "DurableCall", "DurableCallWithHeartbeat",
	"DurableCallWithRetry", "DurableDefer", "DurableDeferFunc", "ListCrons",
	"PluginCall", "PluginCallStreaming", "PollCancellation", "PollChild",
	"PollSignal", "RunID", "ScheduleCron", "SideEffect", "WorkflowID",
}

// hostCallStatus is what a single host call did in a single language.
//
// "suspended" is a first-class outcome and not an error. Suspending is correct
// behaviour for several of these calls, and the engine reports it through a
// separate return value rather than through err -- see executeOneCall.
type hostCallStatus string

const (
	statusOK        hostCallStatus = "ok"
	statusError     hostCallStatus = "error"
	statusSuspended hostCallStatus = "suspended"
	// statusUnsupported: the SDK offers no binding for this host call, so the
	// fixture could not make it. Added for C2, where the Rust SDK turned out
	// to declare neither cron import.
	//
	// A first-class status rather than an "error" with a distinctive message,
	// because the two are different facts: an error is a binding that ran and
	// was refused, and this is a binding that does not exist. Collapsing them
	// puts a missing SDK feature in the same bucket as a host refusal, which
	// is the distinction the harness exists to draw.
	//
	// It also reddens usefully. The day an SDK gains the binding, its row
	// starts reporting ok or error, stops matching the table, and somebody has
	// to decide what the right answer is -- rather than the gap closing
	// silently and the table going on describing a world that no longer
	// exists.
	statusUnsupported hostCallStatus = "unsupported"
)

// hostCallOutcome is the fixture's report for one call.
type hostCallOutcome struct {
	Call   string         `json:"call"`
	Status hostCallStatus `json:"status"`
	Detail string         `json:"detail"`
}

// expectedOutcome is one row of the table. The `why` is the load-bearing
// field: §3.303's lesson is that "a key is present" passed over 16 failures
// and "a reason that matches, with a why" did not.
type expectedOutcome struct {
	status hostCallStatus
	// detailContains is a substring the detail must contain. Empty means the
	// detail is not asserted, which is a weaker row and should be rare: for an
	// "error" row it is the host's message text, and recording only ok/error
	// is precisely what would have missed §3.200, a guest that decoded the
	// host's error length and threw the message away.
	detailContains string
	// detailRegex asserts the SHAPE of a detail that is not stable run to run
	// -- a generated UUID, a child run ID with a random suffix. Seven of the
	// 23 Go rows are in this position (measured 2026-09-05 by diffing two
	// recording runs), and the alternative was to assert nothing about them,
	// which is how a row stops being able to fail.
	detailRegex string
	why         string
}

// executeOneCall runs the fixture for exactly one host call.
//
// It goes to engine.Execute directly rather than through wasmtest.Execute,
// because wasmtest.Execute REPORTS A SUSPENDED WORKFLOW AS A SUCCESSFUL ONE:
// it t.Logf's the suspension and returns a nil error with whatever partial
// result exists (cleat/wasmtest/wasmtest.go:634). For a harness whose job is
// to distinguish outcomes per call, "suspended" arriving as "ok with an empty
// result" is the exact failure this file exists to prevent.
func executeOneCall(t *testing.T, eng *engine.Engine, wasmBytes []byte, call string) hostCallOutcome {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	input, err := json.Marshal(map[string]string{"call": call})
	if err != nil {
		t.Fatalf("%s: marshalling fixture input: %v", call, err)
	}

	result, _, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "exercise_host_call", input)
	switch {
	case err != nil:
		// A trap or an engine-level refusal: the fixture never got to say
		// anything. Distinct from the fixture reporting an "error" outcome,
		// and prefixed so the two cannot be confused in a table row.
		return hostCallOutcome{Call: call, Status: statusError, Detail: "engine: " + err.Error()}
	case suspended != nil:
		return hostCallOutcome{Call: call, Status: statusSuspended, Detail: suspended.Reason}
	}

	var got hostCallOutcome
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("%s: fixture returned undecodable result: %v\nraw: %.500s", call, err, result)
	}
	// The fixture is asked for one call and must answer about that call. A
	// mismatch means dispatch is wrong, and every row would be attributed to
	// the wrong name.
	if got.Call != call {
		t.Fatalf("asked the fixture for %q and it answered about %q", call, got.Call)
	}
	return got
}

// assertOutcome checks one measured outcome against its table row.
func assertOutcome(t *testing.T, lang string, got hostCallOutcome, want expectedOutcome, known bool) {
	t.Helper()

	if !known {
		t.Errorf("%s/%s: no row in the expected-outcome table.\n"+
			"got status=%s detail=%q\n"+
			"Add a row WITH A REASON. A call with no row is a call whose "+
			"behaviour nobody has decided is correct.", lang, got.Call, got.Status, got.Detail)
		return
	}

	if got.Status != want.status {
		t.Errorf("%s/%s: status %s, table says %s (%s)\ndetail: %q",
			lang, got.Call, got.Status, want.status, want.why, got.Detail)
		return
	}
	if want.detailRegex != "" {
		re, err := regexp.Compile(want.detailRegex)
		if err != nil {
			t.Fatalf("%s/%s: table row has an uncompilable detailRegex %q: %v",
				lang, got.Call, want.detailRegex, err)
		}
		if !re.MatchString(got.Detail) {
			t.Errorf("%s/%s: status %s as expected, but the detail does not "+
				"match the shape on file.\nwant match: %s\ngot:        %q\nreason: %s",
				lang, got.Call, got.Status, want.detailRegex, got.Detail, want.why)
		}
	}
	if want.detailContains != "" && !strings.Contains(got.Detail, want.detailContains) {
		t.Errorf("%s/%s: status %s as expected, but the detail changed.\n"+
			"want substring: %q\ngot:            %q\nreason on file: %s\n"+
			"The text is asserted on purpose: §3.200 was a guest that decoded "+
			"the host's error length and discarded the message, and a "+
			"status-only assertion cannot see that.",
			lang, got.Call, got.Status, want.detailContains, got.Detail, want.why)
	}
}

// hostCallFixtureDir returns a fixture directory, failing rather than skipping
// when it is absent.
//
// Not a t.Skipf: every fixture here is committed to this repo, so "missing" is
// case (c) of scripts/check-skips.sh -- a precondition always satisfiable in
// this tree, where a skip is a decision not to run the test. The
// toolchain-gated skips stay in the per-language tests, where they are real.
func hostCallFixtureDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(findProjectRoot(t), "tests", "plugin-harness", "testdata", name)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("host-call fixture %s is missing from the tree: %v", dir, err)
	}
	return dir
}

// recordMode dumps measured outcomes instead of asserting them, so the table
// is written from a measurement rather than from a guess.
//
//	CLEAT_HOSTCALL_RECORD=1 go test ./tests/plugin-harness/ -run TestHostCalls -v
//
// It does not disable the assertions -- it runs them and prints alongside, so
// a recording run is never mistaken for a passing one.
func recordMode() bool { return os.Getenv("CLEAT_HOSTCALL_RECORD") == "1" }

func recordOutcome(got hostCallOutcome) {
	fmt.Printf("RECORD\t%s\t%s\t%s\n", got.Call, got.Status, got.Detail)
}
