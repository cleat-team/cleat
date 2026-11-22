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
	{"pollUpdate", "DurablePollUpdate"},
	{"completeUpdate", "DurableCompleteUpdate"},
	// The plain call goes through step, so its refusal is the dispatcher's and the host method is the site (cleat#4152, G3).
	{"cleatCall", "DurableCall"},
	// sideEffect(Supplier) asks cleat_side_effect_recorded first (cleat#4113); a fresh lookup in a stopped workflow is refused.
	{"sideEffectOnce", "SideEffectRecorded"},
	{"cleatCallWithRetry", "DurableCallWithRetry"},
	// The same host function from inside its attempt loop (cleat#4210): one SDK method, two host sites, one sentinel.
	{"cleatCallWithRetry", "freshCallWithRetry"},
	// The heartbeat call goes through step, so its refusal is the dispatcher's and the host method is the site (cleat#4152, G3).
	{"cleatCallHeartbeat", "DurableCallWithHeartbeat"},
	{"childWorkflow", "childWorkflowWithVersion"},
	{"childWorkflowWithOptions", "childWorkflowWithVersion"},
	// plugin_call goes through step, so its refusal is the dispatcher's and the host method is the site (cleat#4152, G3).
	{"pluginCall", "PluginCall"},
	// A second caller of the same import, so two entries share one host site.
	{"pluginCallOutcome", "PluginCall"},
	{"pluginCallStreaming", "freshPluginCallStreaming"},
	{"awaitSignalsMs", "DurableAwaitSignals"},
	// AwaitPromise is a stop site through suspendingAwaitResult rather than
	// stopBeforeNewWork (cleat#3383): a fresh await that has to wait answers with
	// the sentinel, and bit 31 lands inside this layout's timed-out field.
	{"awaitPromiseMs", "AwaitPromise"},
	// waitForEventMs, not waitForEvent: the latter delegates, so the stop guard
	// lives in the _ms form -- the same shape as awaitSignalsMs above.
	{"waitForEventMs", "WaitForEvent"},
	{"acquireLockMs", "AcquireLock"},
	// deleteCron joined in cleat#4083: a terminated workflow's defer segment
	// could delete a schedule the workflow never reached. The refusal reads as
	// errCode 0 in the simple layout, which is a successful delete.
	{"deleteCron", "DeleteCron"},
	// continueAsNew and continueAsNewVersioned joined in cleat#4159: a terminated workflow's defer segment recorded the body's continuation
	// in the history of a workflow being terminated, and a guest that read the refusal as an error code (the simple layout reads a stop as
	// errCode 0, a success) went on into its own defer drain, where the cleanup's call was refused and the defer table consumed.
	{"continueAsNew", "ContinueAsNew"},
	{"continueAsNewVersioned", "ContinueAsNewWithVersion"},
	// createPromise and setScope joined in the cleat#4079 audit: a terminated
	// workflow's defer segment created a promise row and took a 24-hour scope key
	// from a real store. Both layouts read a refusal as errCode 0, a success.
	{"createPromise", "CreatePromise"},
	// The host site is SetScope, which goes through step (cleat#4211): the dispatcher holds the check for taking a scope. Clearing a scope
	// (SetScope with both names empty) is not a step and is never refused.
	{"setScope", "SetScope"},
	// resolvePromise and rejectPromise joined in cleat#4079, for the same reason: the simple layout reads a stop as error code 0,
	// which is a successful settlement. replyToSignal is a composite over resolvePromise and needs no entry of its own.
	{"resolvePromise", "ResolvePromise"},
	{"rejectPromise", "RejectPromise"},
	{"signalWorkflow", "SignalWorkflow"},
	{"cleatSend", "DurableSend"},
	{"scheduleInvokeMs", "DurableScheduleInvoke"},
	// sendSignalAndWaitMs was here until 2026-09-06. It is no longer a host
	// call: IMPROVEMENT-PLAN 3.220 made it a composite over createPromise +
	// signalWorkflow + awaitPromise, so there is no @Import of its own for
	// the host to refuse, and this guard said so by name ("makes no
	// `long result = ...(` host call, so this entry is stale").
	//
	// Removing it does NOT weaken the guard, and that was checked. Of the
	// three callees, only SignalWorkflow called stopBeforeNewWork; AwaitPromise
	// joined it as a stop site in cleat#3383, through suspendingAwaitResult,
	// and is listed above as awaitPromiseMs. CreatePromise and ResolvePromise
	// still refuse nothing, which is why they were never listed. Every
	// refusable call the composite makes is covered.
	//
	//	for f in CreatePromise AwaitPromise ResolvePromise SignalWorkflow; do
	//	  awk "/func \(s \*execSession\) $f\(/,/^}$/" engine/promises.go \
	//	    engine/signaller.go | grep -c stopBeforeNewWork; done
	//	# 0 0 0 1
	{"sideEffect", "SideEffect"},
	{"cleatFetch", "Fetch"},
	// Both detached entry points name the UNEXPORTED runDetached, which is
	// where stopBeforeNewWork is consulted; RunDetached and StartDetached are
	// thin wrappers over it and consult nothing themselves (cleat#1154).
	// TestEverySDKCoversEveryHostStopSite checks that the name in this column
	// really is a host stop site, and it caught both of these named wrong.
	{"runDetached", "runDetached"},
	{"startDetached", "runDetached"},
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
	// The identifier is CAPTURED, not assumed to be named `result`.
	// AssemblyScript's sideEffect takes a parameter called `result`, so its
	// local is `hostResult`; a fixed name reported that guarded method as
	// unguarded. The worse direction is the silent one: a method that guarded
	// one word while decoding another would have PASSED. Requiring the same
	// identifier in both closes it.
	guard := regexp.MustCompile(`Memory\.throwIfStopped\((\w+)\)`)
	decode := regexp.MustCompile(`Memory\.decode\w+\((\w+)\)`)

	for _, c := range javaCallsTheHostCanRefuse {
		method := c.sdk
		body := javaMethodBody(t, src, method)
		if rawCall.FindStringIndex(body) == nil {
			t.Errorf("%s makes no `long result = ...(` host call, so this entry is stale",
				method)
			continue
		}
		gi := guard.FindStringSubmatchIndex(body)
		if gi == nil {
			t.Errorf("%s never calls Memory.throwIfStopped, so the host can refuse this call "+
				"and the guest will decode the refusal as an ordinary result", method)
			continue
		}
		guarded := body[gi[2]:gi[3]]
		di := decode.FindStringSubmatchIndex(body)
		if di != nil {
			decoded := body[di[2]:di[3]]
			if decoded != guarded {
				t.Errorf("%s guards %q but decodes %q. The guard has to test the WORD THE\nDECODER READS; testing a different local passes this check and lets the refusal\nthrough untouched.", method, guarded, decoded)
			}
			if di[0] < gi[0] {
				t.Errorf("%s decodes a field before calling Memory.throwIfStopped. Order is the "+
					"contract: in the await-signals layout bit 31 lands inside the timed-out "+
					"field, so decoding first turns a stop into an ordinary timeout and the "+
					"workflow runs on -- doing the new work the defer segment exists to prevent, "+
					"with nothing to see.", method)
			}
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
	// A host call that goes through the dispatcher (cleat#4152) has no `if s.stopBeforeNewWork() {` of its own: step applies it to every
	// effectEffect step. Each such call is still one stop site a guest must check, so it counts here, and the count of the calls moved
	// onto step is the same as before they were. Matched at the start of a code line so a comment quoting the call is not counted; a
	// call written any other way is missed, which makes the count LOWER and fails this test, the safe direction.
	//
	// A call whose spec says `Refuse: neverRefuse` (ReleaseLock, cleat#4211) is NOT a stop site: step never refuses it, so there is no refusal for
	// an SDK to check. Counting every `return s.step(` line gave 29 when the true number was 28, and the constant below was one edit from
	// recording the wrong one. The spec is read from the call, as stop_correspondence_guard_test.go's neverRefusedSpecs reads the declaration.
	viaStep := regexp.MustCompile(`(?m)^\s*return s\.step\(\s*[^,()]+,\s*[^,()]+,\s*&(\w+)`)
	unrefused := neverRefusedSpecs(t)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		stopSites += len(call.FindAll(b, -1))
		if e.Name() != "step_dispatcher.go" {
			for _, m := range viaStep.FindAllSubmatch(b, -1) {
				if !unrefused[string(m[1])] {
					stopSites++
				}
			}
		}
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
	// Moved 13 -> 16 on 2026-09-04 for IMPROVEMENT-PLAN 3.300 (the three
	// string-returning calls). Java needed two of the three: sendSignalAndWaitMs
	// and sideEffect both went straight to Memory.decodeSimpleErrCode, where a
	// stop reads as errCode=0 with extra=0 -- an empty SUCCESSFUL response. The
	// third, ScheduleCron, is exempt: the Java SDK has no scheduleCron method at
	// all, so there is nothing to guard. See sdkStopSiteExemptions.
	// 15 since 2026-09-06: SendSignalAndWait's stop site went with the host
	// call itself (IMPROVEMENT-PLAN 3.220). Re-derive with
	//	grep -rn "if s.stopBeforeNewWork() {" engine/*.go | grep -v _test | wc -l
	//
	// 17 since the end-to-end update implementation (IMPROVEMENT-PLAN 3.239):
	// DurablePollUpdate and DurableCompleteUpdate both consult it, because
	// delivering an update runs guest code that can start new work and
	// completing one settles a caller's promise.
	//
	// The other side was checked rather than assumed, which is what this
	// constant is for. Java needs NO new method here: the Java SDK declares
	// neither cleat_poll_update nor cleat_complete_update, so a Java guest
	// cannot reach either call and has no decoder to guard. Both are exempt in
	// sdkStopSiteExemptions["java"], and those exemptions carry an absentToken
	// so that adding the import fails TestEverySDKCoversEveryHostStopSite
	// rather than being silently covered by the exemption.
	//
	// Note the pattern: the message below suggests
	// `grep -rn "stopBeforeNewWork()"`, which also matches the function's own
	// declaration and returns 18. The count here is call SITES.
	// 18 since cleat#1145: DurableCallWithRetry consults it a SECOND time, on
	// the path that resumes a retry policy a crash interrupted. Resuming runs
	// the remaining attempts, which is new work, so it has to be refusable in a
	// defer segment exactly as the fresh path is.
	//
	// The other side was checked rather than assumed. Java needs NO new method:
	// this is a second site for cleat_call_with_retry, which the Java SDK
	// already declares and already guards -- javaCallsTheHostCanRefuse covers
	// the CALL, not the site. A count of sites and a list of methods are
	// different things, and this is the case that separates them.
	//
	// 19 since cleat#4083 (DeleteCron), then 21 since the cleat#4079 audit (CreatePromise and
	// SetScope), each measured against a real store. Both sides were checked rather than assumed,
	// and both needed code. DeleteCron: Java's deleteCron went straight to
	// Memory.decodeSimpleErrCode, where a stop reads as errCode 0, a SUCCESSFUL delete. The audit's
	// createPromise and setScope decoded straight through the same way. Each got a list entry above
	// AND a Memory.throwIfStopped guard in HostCalls.java, not only this number, and so did Rust and
	// the Go adapters. Python and TypeScript need nothing in code: durable-delete-cron has no return
	// value, and the audit's two WIT functions return a plain string, so a component guest cannot
	// receive the refusal (the refused call has no effect); see reasonWitReturnsNothing and
	// reasonWitReturnsAPlainString.
	//
	// 23 since cleat#4079 (this change): ResolvePromise and RejectPromise consult it, because a defer segment's body settling a promise is an
	// effect another run may be waiting on. Checked on the other side as for DeleteCron: Java's resolvePromise and rejectPromise went
	// straight to Memory.decodeSimpleErrCode, where a stop reads as errCode 0, a SUCCESSFUL settlement, so both list entries above
	// and the Memory.throwIfStopped guards in HostCalls.java were added. Rust got the matching stop_requested guards. Python,
	// TypeScript and the Go adapters already decode bit 31 (_check_host_result, withSuspendCheck).
	//
	// 24 since cleat#4113 (SideEffectRecorded): a fresh lookup in a stopped workflow is refused so a guest's side-effect function never runs.
	// Java covers it with sideEffectOnce (cleat#4113), which asks first and checks the stop bit on the lookup; the exemption the engine PR added
	// for Java is deleted by the Java PR.
	//
	// 25 since cleat#4143 (SideEffectRecorded, a SECOND site): the lookup is refused at the replay frontier of a defer segment too, because
	// a segment over a run with history starts in replay and an SDK that asks first runs its function between the lookup and the record.
	// Both sides were checked rather than assumed. Java needs NO new method for the second site: it is the same host call, and sideEffectOnce's
	// stop check on the lookup covers the refusal at either site, the fresh one and the replay-frontier one. A count of sites and a list of methods are
	// different things, as at 18.
	// 26 since cleat#4152 (DurableCallWithRetry, a further site, counted after cleat#4143's second): the replay-frontier block refuses a defer segment inside the block, so that
	// the refusal sits in the branch that spells the frontier predicate. Same host call as the sites already counted, so Java needs no
	// new method; a count of sites and a list of methods are different things, as at 18.
	//
	// 28 since cleat#4159 (ContinueAsNew and ContinueAsNewWithVersion): a defer segment's body must not continue as new. Both sides were
	// checked rather than assumed, and both needed code: Java's continueAsNew and continueAsNewVersioned went straight to
	// Memory.decodeSimpleErrCode, where a stop reads as errCode 0, a SUCCESS, so both list entries above and Memory.throwIfStopped guards
	// in HostCalls.java were added. Rust got the matching stop_requested guards, Go's adapters withSuspendCheck. Python decodes bit 31
	// (_check_host_result) and C# tests the sentinel first (Packed.CheckHostResult); neither needed code.
	// 26 since cleat#4152 G3 (DurableCallWithRetry moved onto the dispatcher): the call had THREE `if s.stopBeforeNewWork() {` sites of its own (the replay frontier,
	// the resume of a policy a crash interrupted, and the fresh path: the 18 and the 26 above) and now has ONE, step's, counted through `return s.step(`: 28 - 3 + 1.
	// Both sides were checked rather than assumed. It is the same host call, cleat_call_retry, which the Java SDK already declares and already guards
	// (javaCallsTheHostCanRefuse covers the CALL, not the site), so a site REMOVED from the engine drops no Java method and needs no code: the number of
	// methods stays 30. A count of sites and a list of methods are different things, as at 18. (The retry loop's own check between attempts is not an
	// `if s.stopBeforeNewWork() {` line, and is not counted here or before.)
	const stopSitesOn20260904 = 26
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
