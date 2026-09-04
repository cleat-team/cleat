package engine

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Java SDK carries its own copy of the defer-segment stop sentinel, because
// nothing in either language can see the other: the engine cannot import Java
// and the SDK cannot import Go. So the checks have to run from this side, by
// reading the file. Same shape as cleat/host_retry_budget_parity_test.go, which
// reads the Rust SDK for the retry budget.
//
// A drifted or missing sentinel is worse than no sentinel: the host refuses a
// call and the guest decodes the refusal through whichever layout it expected,
// and every one of those readings is a plausible ordinary result.

const javaMemorySrc = "../crates/cleat-java/src/main/java/cleat/Memory.java"
const javaHostCallsSrc = "../crates/cleat-java/src/main/java/cleat/HostCalls.java"

func readJavaSDK(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"These tests hold the Java SDK to the engine's stop sentinel by reading the "+
			"Java source. If the SDK moved, re-point them -- do not delete them, because "+
			"the two copies have nothing else holding them together.", path, err)
	}
	return string(src)
}

func TestTheJavaSDKAgreesOnTheStopBit(t *testing.T) {
	src := readJavaSDK(t, javaMemorySrc)

	re := regexp.MustCompile(`(?m)^\s*public static final long SUSPEND_STOP_BIT = 1L << (\d+);`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `public static final long SUSPEND_STOP_BIT = 1L << N;` in %s.\n\n"+
			"A regex that matches nothing passes vacuously, so this is a failure rather "+
			"than a skip. Either the constant was renamed -- re-point this test -- or the "+
			"Java SDK stopped decoding the stop sentinel, in which case 'java' must come "+
			"out of deferSegmentLanguages in the same change.", javaMemorySrc)
	}

	// Compare shifts rather than values: comparing the rendered constant would
	// pass for a differently-spelled equal value while telling the reader
	// nothing about which bit moved.
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
		t.Errorf("the Java SDK's SUSPEND_STOP_BIT is bit %s, the engine's callSuspendSentinel "+
			"is bit %d", got, wantShift)
	}
}

// javaCallsTheHostCanRefuse are the Java SDK methods whose host function calls
// stopBeforeNewWork(), so the host can set bit 31 on their result and the guest
// MUST test it before decoding any field.
//
// This list is not "every host call". Guarding a call the host never refuses is
// harmless only where bit 31 is unreachable in that call's layout, and it is
// reachable in one of them -- see javaCallsThatMustNotCheck. So the rule is
// stated as a list rather than as "everywhere", and
// TestTheRequiredJavaGuardsCoverEveryHostStopSite keeps it honest against the
// engine.
var javaCallsTheHostCanRefuse = []sdkRefusableCall{
	{"cleatCall", "DurableCall"},
	{"cleatCallWithRetry", "DurableCallWithRetry"},
	{"cleatCallHeartbeat", "DurableCallWithHeartbeat"},
	{"childWorkflow", "childWorkflowWithVersion"},
	{"childWorkflowWithOptions", "childWorkflowWithVersion"},
	{"pluginCall", "PluginCall"},
	// A second caller of the same import, so two entries share one host site.
	{"pluginCallOutcome", "PluginCall"},
	{"pluginCallStreaming", "PluginCallStreaming"},
	{"awaitSignalsMs", "DurableAwaitSignals"},
	{"acquireLockMs", "AcquireLock"},
	{"signalWorkflow", "SignalWorkflow"},
	{"cleatSend", "DurableSend"},
	{"scheduleInvokeMs", "DurableScheduleInvoke"},
	{"cleatFetch", "Fetch"},
	{"runDetached", "RunDetached"},
}

// javaCallsThatMustNotCheck are the methods where the guard would be a defect
// rather than an omission, with the reason.
var javaCallsThatMustNotCheck = map[string]string{
	"cleatSleepMs": "packSleepResult is the one layout in which bit 31 is REACHABLE -- " +
		"TestStopSentinelBitsAcrossEveryLayout measures it as reachable=ff0000fffffffc00 -- " +
		"so a guard here would read an ordinary sleep result as a stop. It is also the one " +
		"call that does not need a sentinel: a sleeping guest suspends through its own " +
		"status byte and never reaches a fresh call " +
		"(TestASleepingWorkflowNeverReachesAFreshCall).",
}

// javaMethodBody returns the source of the named Java method, from its
// declaration to the start of the next one.
func javaMethodBody(t *testing.T, src, method string) string {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^\s*public\s[\w<>,\[\] ]*\b` + regexp.QuoteMeta(method) + `\s*\(`)
	loc := decl.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no `public ... %s(` in %s.\n\n"+
			"A lookup that finds nothing passes vacuously. Either the method was renamed -- "+
			"update javaCallsTheHostCanRefuse -- or it was deleted, in which case check "+
			"whether the host call it made went with it.", method, javaHostCallsSrc)
	}
	rest := src[loc[1]:]
	if next := regexp.MustCompile(`(?m)^\s*(public|private|protected)\s`).FindStringIndex(rest); next != nil {
		return rest[:next[0]]
	}
	return rest
}

func TestEveryJavaCallTheHostCanRefuseChecksTheStopBit(t *testing.T) {
	src := readJavaSDK(t, javaHostCallsSrc)

	rawCall := regexp.MustCompile(`(?m)^\s*long result = \w+\(`)
	guard := regexp.MustCompile(`Memory\.throwIfStopped\(result\)`)
	decode := regexp.MustCompile(`Memory\.decode\w+\(result\)`)

	for _, c := range javaCallsTheHostCanRefuse {
		method := c.sdk
		body := javaMethodBody(t, src, method)
		if rawCall.FindStringIndex(body) == nil {
			t.Errorf("%s makes no `long result = ...(` host call, so this entry is stale",
				method)
			continue
		}
		gi := guard.FindStringIndex(body)
		if gi == nil {
			t.Errorf("%s never calls Memory.throwIfStopped, so the host can refuse this call "+
				"and the guest will decode the refusal as an ordinary result", method)
			continue
		}
		if di := decode.FindStringIndex(body); di != nil && di[0] < gi[0] {
			t.Errorf("%s decodes a field before calling Memory.throwIfStopped. Order is the "+
				"contract: in the await-signals layout bit 31 lands inside the timed-out "+
				"field, so decoding first turns a stop into an ordinary timeout and the "+
				"workflow runs on -- doing the new work the defer segment exists to prevent, "+
				"with nothing to see.", method)
		}
	}
}

func TestTheJavaSleepPathDoesNotCheckTheStopBit(t *testing.T) {
	src := readJavaSDK(t, javaHostCallsSrc)
	for method, why := range javaCallsThatMustNotCheck {
		body := javaMethodBody(t, src, method)
		if strings.Contains(body, "Memory.throwIfStopped") {
			t.Errorf("%s calls Memory.throwIfStopped, and it must not.\n\n%s", method, why)
		}
	}
}

// The list above is a hand-maintained mirror of the engine's stop sites, so it
// can go stale in the direction that fools you: a new stopBeforeNewWork() call
// in the engine means a new Java method must guard, and nothing else would say
// so.
func TestTheRequiredJavaGuardsCoverEveryHostStopSite(t *testing.T) {
	// Re-derived from the engine rather than hardcoded, so adding a stop site
	// fails here rather than being noticed later by a guest that ran on.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading engine dir: %v", err)
	}
	stopSites := 0
	call := regexp.MustCompile(`(?m)^\s*if s\.stopBeforeNewWork\(\) \{`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		stopSites += len(call.FindAll(b, -1))
	}
	if stopSites == 0 {
		t.Fatal("found no `if s.stopBeforeNewWork() {` sites in engine/, so this test is " +
			"matching nothing and reporting green -- re-derive the pattern")
	}
	// An EXACT count, not a floor. The first version of this compared
	// len(javaCallsTheHostCanRefuse) >= stopSites, which passes with slack --
	// ten Java methods cover seven sites because PluginCall has two callers, so
	// three new stop sites could land before it fired. A gate with slack is a
	// gate that reports green through the change it exists to catch, which is
	// what falsifying it showed: adding an eighth site left it passing.
	//
	// So the number is pinned and dated instead. Changing it is the point: a
	// new stop site fails here, and the fix is to add the Java method that
	// reaches it to javaCallsTheHostCanRefuse, add the guard in HostCalls.java,
	// and then move this number.
	//
	// Moved 7 -> 8 on 2026-09-04 for IMPROVEMENT-PLAN 3.111, and the check the
	// message above demands was done rather than assumed: `cleatCallHeartbeat`
	// was ALREADY in javaCallsTheHostCanRefuse and HostCalls.java already had
	// `Memory.throwIfStopped(result)` ahead of `decodeCallErrCode`, so Java
	// needed nothing. The gap was entirely host-side, which is why only the
	// number moved -- and this test is the reason that was verified instead of
	// assumed from the Java list being long enough.
	//
	// Moved 9 -> 10 on 2026-09-04 for IMPROVEMENT-PLAN 3.301 (AcquireLock), and
	// this time Java DID need something: `acquireLockMs` decoded straight to
	// `result & 0xFFL`, so a stop would have read as errCode=0 with `acquired`
	// false at bit 8 -- an ordinary "someone else holds the lock". Both halves
	// were added here, not just the number.
	// Moved 10 -> 13 on 2026-09-04 for IMPROVEMENT-PLAN 3.302 (the three
	// fire-and-forget calls). Java needed all three: signalWorkflow, cleatSend
	// and scheduleInvokeMs each went straight to Memory.decodeSimpleErrCode,
	// which reads the low byte -- and a stop is 0 there, which for a
	// fire-and-forget call is an ordinary SUCCESS. The guest would have reported
	// the send as done.
	const stopSitesOn20260904 = 13
	if stopSites != stopSitesOn20260904 {
		t.Errorf("the engine has %d `if s.stopBeforeNewWork() {` sites; this test was written "+
			"against %d.\n\nIf a site was ADDED, the Java SDK has a call the host can now "+
			"refuse and does not check: add its method to javaCallsTheHostCanRefuse and the "+
			"guard to HostCalls.java, then update this constant. If one was REMOVED, drop the "+
			"corresponding method. Re-derive with:\n\n"+
			"    grep -rn \"stopBeforeNewWork()\" --include=\"*.go\" engine/ | grep -v _test.go\n\n"+
			"Do not just move the number -- it is the prompt to check the other side.",
			stopSites, stopSitesOn20260904)
	}
	t.Logf("engine stop sites: %d; java methods required to guard: %d",
		stopSites, len(javaCallsTheHostCanRefuse))
}
