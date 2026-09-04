package engine

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The AssemblyScript half of the defer-segment stop sentinel, held from this
// side because nothing in either language can see the other. Companion to
// engine/java_sdk_stop_bit_parity_test.go; the two SDKs need the same checks
// for the same reason and differ only in how a stop is signalled once decoded.
//
// AssemblyScript has no exceptions (--runtime stub), so it cannot unwind the
// way Go, Java and Rust do. A stop sets the suspension flag and returns an
// error result, and the workflow body can ignore both. What makes that
// acceptable rather than a hole is that the host refuses EVERY call for the
// rest of the segment, so a guest that runs on cannot reach anything durable --
// see the note on stopRequested in packages/cleat-as/assembly/memory.ts.

const asMemorySrc = "../packages/cleat-as/assembly/memory.ts"
const asHostCallsSrc = "../packages/cleat-as/assembly/host-calls.ts"

func readASSDK(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"These tests hold the AssemblyScript SDK to the engine's stop sentinel by "+
			"reading its source. If the SDK moved, re-point them -- do not delete them, "+
			"because the two copies have nothing else holding them together.", path, err)
	}
	return string(src)
}

func TestTheAssemblyScriptSDKAgreesOnTheStopBit(t *testing.T) {
	src := readASSDK(t, asMemorySrc)

	re := regexp.MustCompile(`(?m)^\s*export const SUSPEND_STOP_BIT: i64 = 0x([0-9A-Fa-f]+);`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `export const SUSPEND_STOP_BIT: i64 = 0x...;` in %s.\n\n"+
			"A regex that matches nothing passes vacuously, so this is a failure rather "+
			"than a skip. Either the constant was renamed -- re-point this test -- or the "+
			"SDK stopped decoding the stop sentinel, in which case 'assemblyscript' must "+
			"not be added to deferSegmentLanguages.", asMemorySrc)
	}
	got, err := strconv.ParseInt(m[1], 16, 64)
	if err != nil {
		t.Fatalf("SUSPEND_STOP_BIT is 0x%s, which does not parse as an int64: %v", m[1], err)
	}
	if got != callSuspendSentinel {
		t.Errorf("the AssemblyScript SDK's SUSPEND_STOP_BIT is %#x, the engine's "+
			"callSuspendSentinel is %#x.\n\nA drifted sentinel is worse than no sentinel: "+
			"the host refuses a call and the guest decodes the refusal through whichever "+
			"layout it expected, and every one of those readings is a plausible ordinary "+
			"result.", got, callSuspendSentinel)
	}
}

// asCallsTheHostCanRefuse are the AssemblyScript methods whose host function
// calls stopBeforeNewWork(), so the host can set bit 31 on their result.
//
// Same rule as the Java list, and the same reason it is a list rather than
// "every host call": bit 31 is REACHABLE in packSleepResult, so a guard on the
// sleep path would be a defect. See asCallsThatMustNotCheck.
var asCallsTheHostCanRefuse = []sdkRefusableCall{
	{"cleatCallMs", "DurableCall"},
	{"cleatCallRetry", "DurableCallWithRetry"},
	{"cleatCallHeartbeat", "DurableCallWithHeartbeat"},
	{"childWorkflow", "childWorkflowWithVersion"},
	{"childWorkflowWithOptions", "childWorkflowWithVersion"},
	{"pluginCall", "PluginCall"},
	{"pluginCallStreaming", "PluginCallStreaming"},
	{"awaitSignalsMs", "DurableAwaitSignals"},
	{"acquireLockMs", "AcquireLock"},
	{"cleatFetch", "Fetch"},
	{"runDetached", "RunDetached"},
}

var asCallsThatMustNotCheck = map[string]string{
	"cleatSleepMs": "packSleepResult is the one layout in which bit 31 is REACHABLE -- " +
		"TestStopSentinelBitsAcrossEveryLayout measures it as reachable=ff0000fffffffc00 -- " +
		"so a guard here would read an ordinary sleep result as a stop. It is also the one " +
		"call that does not need a sentinel: a sleeping guest suspends through its own " +
		"status byte and never reaches a fresh call.",
}

// asMethodBody returns the source of the named method, from its declaration to
// the start of the next one at the same indentation.
func asMethodBody(t *testing.T, src, method string) string {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(method) + `[(<]`)
	loc := decl.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no `  %s(` in %s.\n\n"+
			"A lookup that finds nothing passes vacuously. Either the method was renamed -- "+
			"update asCallsTheHostCanRefuse -- or it was deleted, in which case check "+
			"whether the host call it made went with it.", method, asHostCallsSrc)
	}
	rest := src[loc[1]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z_]\w*[(<]`).FindStringIndex(rest); next != nil {
		return rest[:next[0]]
	}
	return rest
}

func TestEveryASCallTheHostCanRefuseChecksTheStopBit(t *testing.T) {
	src := readASSDK(t, asHostCallsSrc)

	guard := regexp.MustCompile(`stopRequested\(result\)`)
	decode := regexp.MustCompile(`decode\w+\(result\)`)

	for _, c := range asCallsTheHostCanRefuse {
		method := c.sdk
		body := asMethodBody(t, src, method)
		gi := guard.FindStringIndex(body)
		if gi == nil {
			t.Errorf("%s never calls stopRequested, so the host can refuse this call and the "+
				"guest will decode the refusal as an ordinary result", method)
			continue
		}
		if di := decode.FindStringIndex(body); di != nil && di[0] < gi[0] {
			t.Errorf("%s decodes a field before calling stopRequested. Order is the contract: "+
				"in the await-signals layout bit 31 lands inside the timed-out field, so "+
				"decoding first turns a stop into an ordinary timeout and the workflow runs "+
				"on -- doing the new work the defer segment exists to prevent, with nothing "+
				"to see.", method)
		}
	}
}

func TestTheASSleepPathDoesNotCheckTheStopBit(t *testing.T) {
	src := readASSDK(t, asHostCallsSrc)
	for method, why := range asCallsThatMustNotCheck {
		body := asMethodBody(t, src, method)
		if strings.Contains(body, "stopRequested(") {
			t.Errorf("%s calls stopRequested, and it must not.\n\n%s", method, why)
		}
	}
}
