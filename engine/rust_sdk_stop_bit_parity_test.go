package engine

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Rust half of the defer-segment stop sentinel, held from this side for the
// same reason as the Java and AssemblyScript versions: nothing in either
// language can see the other.
//
// Rust is the strongest of the three. Go panics and Java throws; AssemblyScript
// can only set a flag (§3.106); Rust returns Err(CallError::Suspended) through
// suspend(), which also sets the #[cleat_entry] backstop -- so even the host
// calls that return (String, Option<String>) rather than Result end the segment
// when a workflow body discards the error half. §3.87 (#643) is what made that
// shape possible: before it, suspension was a panic on a target with no
// unwinding.

const rustMemorySrc = "../crates/cleat-sdk/src/memory.rs"
const rustHostCallsSrc = "../crates/cleat-sdk/src/host_calls.rs"

func readRustSDK(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"These tests hold the Rust SDK to the engine's stop sentinel by reading its "+
			"source. If the SDK moved, re-point them -- do not delete them, because the two "+
			"copies have nothing else holding them together.", path, err)
	}
	return string(src)
}

func TestTheRustSDKAgreesOnTheStopBit(t *testing.T) {
	src := readRustSDK(t, rustMemorySrc)

	re := regexp.MustCompile(`(?m)^\s*pub const SUSPEND_STOP_BIT: i64 = 1 << (\d+);`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `pub const SUSPEND_STOP_BIT: i64 = 1 << N;` in %s.\n\n"+
			"A regex that matches nothing passes vacuously, so this is a failure rather "+
			"than a skip. Either the constant was renamed -- re-point this test -- or the "+
			"SDK stopped decoding the stop sentinel, in which case 'rust' must not be added "+
			"to deferSegmentLanguages.", rustMemorySrc)
	}

	wantShift := -1
	for i := 0; i < 64; i++ {
		if callSuspendSentinel == int64(1)<<uint(i) {
			wantShift = i
			break
		}
	}
	if wantShift < 0 {
		t.Fatal("callSuspendSentinel is no longer a single bit; this test compares shifts " +
			"and needs rewriting alongside whatever replaced it")
	}
	if got := m[1]; got != strconv.Itoa(wantShift) {
		t.Errorf("the Rust SDK's SUSPEND_STOP_BIT is bit %s, the engine's callSuspendSentinel "+
			"is bit %d", got, wantShift)
	}
}

// rustCallsTheHostCanRefuse are the Rust SDK functions whose host import calls
// stopBeforeNewWork(), so the host can set bit 31 on their result.
//
// Same rule and same reason for being a list rather than "every host call" as
// the Java and AssemblyScript versions: bit 31 is REACHABLE in packSleepResult,
// so a guard on the sleep path would be a defect.
//
// Eight, not nine: this SDK has no call_with_retry.
var rustCallsTheHostCanRefuse = []sdkRefusableCall{
	{"cleat_call", "DurableCall"},
	{"cleat_call_heartbeat", "DurableCallWithHeartbeat"},
	{"child_workflow", "childWorkflowWithVersion"},
	{"child_workflow_with_options", "childWorkflowWithVersion"},
	{"plugin_call", "PluginCall"},
	{"plugin_call_streaming", "PluginCallStreaming"},
	{"await_signals_ms", "DurableAwaitSignals"},
	{"acquire_lock_ms", "AcquireLock"},
	{"signal_workflow", "SignalWorkflow"},
	{"cleat_send", "DurableSend"},
	{"schedule_invoke_ms", "DurableScheduleInvoke"},
	{"cleat_fetch", "Fetch"},
	{"run_detached", "RunDetached"},
}

var rustCallsThatMustNotCheck = map[string]string{
	"cleat_sleep_ms": "packSleepResult is the one layout in which bit 31 is REACHABLE -- " +
		"TestStopSentinelBitsAcrossEveryLayout measures it as reachable=ff0000fffffffc00 -- " +
		"so a guard here would read an ordinary sleep result as a stop. It is also the one " +
		"call that does not need a sentinel: it already returns Err(suspend()) from its own " +
		"status byte.",
}

// rustFnBody returns the source of the named function, from its declaration to
// the start of the next one at the same indentation.
func rustFnBody(t *testing.T, src, fn string) string {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^    pub fn ` + regexp.QuoteMeta(fn) + `\b`)
	loc := decl.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no `    pub fn %s` in %s.\n\n"+
			"A lookup that finds nothing passes vacuously. Either the function was renamed "+
			"-- update rustCallsTheHostCanRefuse -- or it was deleted, in which case check "+
			"whether the host call it made went with it.", fn, rustHostCallsSrc)
	}
	rest := src[loc[1]:]
	if next := regexp.MustCompile(`(?m)^    pub fn `).FindStringIndex(rest); next != nil {
		return rest[:next[0]]
	}
	return rest
}

func TestEveryRustCallTheHostCanRefuseChecksTheStopBit(t *testing.T) {
	src := readRustSDK(t, rustHostCallsSrc)

	guard := regexp.MustCompile(`stop_requested\(result\)`)
	decode := regexp.MustCompile(`memory::decode_\w+\(result\)`)

	for _, c := range rustCallsTheHostCanRefuse {
		fn := c.sdk
		body := rustFnBody(t, src, fn)
		gi := guard.FindStringIndex(body)
		if gi == nil {
			t.Errorf("%s never calls stop_requested, so the host can refuse this call and the "+
				"guest will decode the refusal as an ordinary result", fn)
			continue
		}
		if di := decode.FindStringIndex(body); di != nil && di[0] < gi[0] {
			t.Errorf("%s decodes a field before calling stop_requested. Order is the contract: "+
				"in the await-signals layout bit 31 lands inside the timed-out field, so "+
				"decoding first turns a stop into an ordinary timeout and the workflow runs "+
				"on -- doing the new work the defer segment exists to prevent, with nothing "+
				"to see.", fn)
		}
	}
}

func TestTheRustSleepPathDoesNotCheckTheStopBit(t *testing.T) {
	src := readRustSDK(t, rustHostCallsSrc)
	for fn, why := range rustCallsThatMustNotCheck {
		body := rustFnBody(t, src, fn)
		if strings.Contains(body, "stop_requested(") {
			t.Errorf("%s calls stop_requested, and it must not.\n\n%s", fn, why)
		}
	}
}

// stop_requested must go through suspend(), not set CallError::Suspended
// itself. suspend() also marks the #[cleat_entry] backstop, and that backstop
// is the only thing that ends the segment when a workflow body discards the
// error half of a (String, Option<String>) return -- which six of the eight
// guarded functions have.
func TestTheRustStopGoesThroughSuspend(t *testing.T) {
	src := readRustSDK(t, rustHostCallsSrc)
	body := regexp.MustCompile(`(?s)fn stop_requested\(result: i64\) -> bool \{.*?\n\}`).FindString(src)
	if body == "" {
		t.Fatalf("no `fn stop_requested(result: i64) -> bool` in %s", rustHostCallsSrc)
	}
	if !strings.Contains(body, "suspend()") {
		t.Errorf("stop_requested does not call suspend().\n\nIt must: suspend() sets the "+
			"#[cleat_entry] backstop, and six of the eight guarded host calls return "+
			"(String, Option<String>) rather than Result, so a workflow body can discard the "+
			"error and keep going. The backstop is what still ends the segment. Body was:\n\n%s",
			body)
	}
}
