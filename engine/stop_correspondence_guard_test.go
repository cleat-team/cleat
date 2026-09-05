package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A host call the host can refuse mid-defer-segment has THREE surfaces, and all
// three have to agree or the refusal is lost on the way to some guest.
//
//	1. the host      -- `if s.stopBeforeNewWork() { return callSuspendSentinel }`
//	2. the Go guest  -- the adapter's decoder wrapped in withSuspendCheck, so it
//	                    tests bit 31 BEFORE reading any field
//	3. the component -- a WIT signature returning result<string, call-failure>,
//	                    so `suspended` is a case of the type rather than a value
//	                    a service could also produce
//
// WHY A GUARD AND NOT A CONVENTION. Four separate changes have each added one of
// these by hand and the inventory came up short three times: §3.83 guarded
// `cleat_call`; §3.84 added four more and its table omitted
// `DurableCallWithHeartbeat` from the very family it was enumerating; §3.104
// added `Fetch`, noting the inventory had been built "by reading the entry
// points a guest uses to reach a service"; §3.111 added the heartbeat as the
// eighth. The count is not the argument. The argument is that the fifth person
// will add one by hand too.
//
// `withSuspendCheck`'s own comment already claims what this test enforces --
// "so the check cannot be forgotten by a decoder that is added later" -- and
// that claim was false the day it was written, because the wrapper is opt-in and
// the heartbeat adapter was the counterexample for weeks.
//
// WHAT ALREADY EXISTS AND WHY THIS IS NOT IT. The Java parity test
// (java_sdk_stop_bit_parity_test.go) pins the host-side count and fires when it
// moves -- it is what caught §3.111 -- but it checks ONE guest language, and
// Rust and AssemblyScript have per-method coverage with no host-side pin at all,
// so a ninth site passes both of them silently. This test is about the
// correspondence rather than about any one SDK.

// exemption reasons. Constants rather than comments, because "why is this
// exempt" has to be answerable by grep, and because a comment gets copied onto
// the next entry without being reread -- at which point two entries appear to
// have the same reason when they do not.
type exemptReason string

const (
	// reasonNoGoAdapter: Go never imports this host function, so there is no
	// decoder to wrap. Not a gap -- the Go guest reaches the same host function
	// through one that IS guarded.
	reasonNoGoAdapter exemptReason = "no-go-adapter"

	// reasonWitIsStillCoreABI: an OPEN FINDING, not a safe exemption. The WIT
	// signature still takes out-pointers into linear memory, so the call has
	// never worked on a component at all and cannot express `suspended` until
	// it is redesigned. Tracked in IMPROVEMENT-PLAN §3.110.
	reasonWitIsStillCoreABI exemptReason = "open-finding-wit-is-core-abi"

	// reasonNotInTheComponentWorld: the host function has no WIT declaration at
	// all, so a component guest cannot call it and there is no signature to
	// carry the refusal. Distinct from reasonWitIsStillCoreABI, which is about a
	// function the world DOES declare and declares wrongly -- keeping them
	// separate is the point of these being constants.
	reasonNotInTheComponentWorld exemptReason = "not-in-the-component-world"
)

// stopSurface declares, for one host stop site, which Go adapters and which WIT
// functions carry the same refusal. Discovery of the SITES is automatic; only
// the naming is declared, because the three surfaces use three naming
// conventions and none is derivable from the others (DurableCallWithRetry is
// `durable-call-retry`, not `durable-call-with-retry`).
type stopSurface struct {
	adapters   []string
	wit        []string
	adapterWhy exemptReason // set when adapters is deliberately empty
	witWhy     exemptReason // set when wit is deliberately empty

	// py names the Python SDK methods that decode this call, and is REQUIRED
	// when the WIT function returns a scalar rather than result<...>.
	//
	// For a result<...> call the component guest cannot misread a stop: the
	// refusal is a case of the return type and there is no other way to read
	// it. A scalar return gives that up -- bit 31 arrives inside an ordinary
	// word -- so something has to assert the SDK tests it, and this is that
	// something. Dropping the WIT requirement without adding this would have
	// removed a structural guarantee and replaced it with nothing.
	py []string
}

var stopSurfaces = map[string]stopSurface{
	"AcquireLock": {
		// The first stop site whose WIT function returns a SCALAR rather than
		// result<string, call-failure>, which is why witCallOutcomeFuncs now
		// reports a third category. §3.113 recorded the rule as wrong for
		// numeric calls and deliberately did not change it, on the grounds
		// that a rule loosened before it has a case is a rule nobody can
		// falsify. This is the case.
		// "AcquireLockMs" was here until 2026-09-05. It named an adapterDefs
		// entry for a field cleat.HostCallsOptions does not have, so the
		// generator emitted `AcquireLockMs:` into the struct literal and no
		// workflow using a lock could compile. cleat_acquire_lock reaches the
		// host through the AcquireLock adapter alone -- HostCallsImpl.AcquireLockMs
		// calls opts.AcquireLock. See IMPROVEMENT-PLAN.md 3.204.
		adapters: []string{"AcquireLock"},
		wit:      []string{"durable-acquire-lock"},
		py:       []string{"acquire_lock_ms"},
	},
	// The three fire-and-forget calls (§3.302). All scalar-returning, so each
	// needs `py`; and none has a Go adapter, for a reason worth stating because
	// it is not the same as Fetch's. Fetch has no adapter because Go reaches it
	// through DurableCall. These have none because they are ABSENT FROM
	// wasm/usage.go, so the scanner never records them as used, no adapterDefs
	// entry is consulted, and no HostCallsOptions field is emitted -- leaving
	// HostCallsImpl.signalWorkflow nil and the SDK method returning its
	// "can only be called from within a workflow function" error. A Go WASM
	// guest cannot reach these host functions at all.
	//
	//	grep -oE '\{"[a-z_]+", "\w+"\}' wasm/usage.go | sort -u
	"SignalWorkflow": {
		adapters:   nil,
		adapterWhy: reasonNoGoAdapter,
		wit:        []string{"durable-signal-workflow"},
		py:         []string{"signal_workflow"},
	},
	"DurableSend": {
		adapters:   nil,
		adapterWhy: reasonNoGoAdapter,
		wit:        []string{"durable-send"},
		py:         []string{"send"},
	},
	"DurableScheduleInvoke": {
		adapters:   nil,
		adapterWhy: reasonNoGoAdapter,
		wit:        []string{"durable-schedule-invoke"},
		py:         []string{"schedule_invoke"},
	},
	// The three string-returning calls (§3.300). Before that change their WIT
	// spelled the return as a bare `string`, which has nowhere to put the
	// sentinel -- so these are the calls §3.113 identified as needing the
	// signature change rather than the scalar rule §3.301 introduced. No `py`
	// field: once the return is result<...>, the component guest cannot misread
	// a stop, which is the guarantee `py` exists to substitute for.
	"SideEffect": {
		adapters: []string{"SideEffect"},
		wit:      []string{"side-effect"},
	},
	"ScheduleCron": {
		adapters: []string{"ScheduleCron"},
		wit:      []string{"durable-schedule-cron"},
	},
	"SendSignalAndWait": {
		// Absent from wasm/usage.go, like the three fire-and-forget calls of
		// §3.302: no adapterDef is consulted, no HostCallsOptions field is
		// emitted, and a Go WASM guest cannot reach this host function.
		adapters:   nil,
		adapterWhy: reasonNoGoAdapter,
		wit:        []string{"durable-send-signal-and-wait"},
	},
	"DurableCall": {
		adapters: []string{"DurableCall"},
		wit:      []string{"durable-call"},
	},
	"DurableCallWithRetry": {
		adapters: []string{"DurableCallWithRetry"},
		wit:      []string{"durable-call-retry"},
	},
	"DurableCallWithHeartbeat": {
		adapters: []string{"DurableCallWithHeartbeat"},
		wit:      []string{"durable-call-heartbeat"},
	},
	// One host method serves both entry points, which is why the method count
	// (8) and the entry-point count (9) differ, and why §3.84's table shows
	// fewer rows than either. Any sentence about "how many paths" has to say
	// which of the three it means.
	"childWorkflowWithVersion": {
		adapters: []string{"ChildWorkflow", "ChildWorkflowWithOptions"},
		wit:      []string{"durable-child-workflow", "durable-child-workflow-with-options"},
	},
	"PluginCall": {
		adapters: []string{"PluginCall"},
		wit:      []string{"plugin-call"},
	},
	"PluginCallStreaming": {
		adapters: []string{"PluginCallStreaming"},
		wit:      []string{"plugin-call-streaming"},
	},
	"Fetch": {
		// cleat/runtime_workflow.go's DurableFetch routes through
		// DurableCall("http", "fetch", ...), which is guarded above, so a Go
		// guest never imports cleat_fetch and there is no adapter to wrap.
		// The exposure §3.104 fixed was to Java and AssemblyScript, which do
		// import it directly.
		adapters:   nil,
		adapterWhy: reasonNoGoAdapter,
		wit:        []string{"fetch"},
	},
	"RunDetached": {
		// No Go adapter and no WIT function: `cleat_run_detached` is imported by
		// the Rust, Java and AssemblyScript SDKs only. Go's cleat.HostCalls has a
		// RunDetached, but it is a different thing -- it runs a closure with a
		// HostCalls that ignores cancellation, and never touches this import.
		adapters:   nil,
		adapterWhy: reasonNoGoAdapter,
		wit:        nil,
		witWhy:     reasonNotInTheComponentWorld,
	},
	"DurableAwaitSignals": {
		adapters: []string{"DurableAwaitSignals"},
		// OPEN. Its WIT signature is
		//   (names, timeout-ms, sig-name-ptr, sig-name-max-len, payload-ptr,
		//    payload-max-len) -> u64
		// -- out-pointers addressing the guest's linear memory, while the
		// component dispatch writes into a HOST buffer. So the Python SDK reads
		// whatever was at OUTPUT_OFFSET: this call has never worked on a
		// component, and the missing `result<string, call-failure>` is a symptom
		// of that rather than a separate omission. §3.110 records it.
		wit:    nil,
		witWhy: reasonWitIsStillCoreABI,
	},
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// hostStopSites returns the execSession methods that consult stopBeforeNewWork.
//
// The definition of stopBeforeNewWork itself contains its own name and is not a
// caller. Excluding it is not cosmetic: including it inflates the count by one,
// and a count is exactly the thing that cannot tell you it counted itself. The
// first hand-run of this scan reported 8 where the answer was 7.
func hostStopSites(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading engine/: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^func \(s \*execSession\) ([A-Za-z_]\w*)\(`)
	sites := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		src := string(b)
		locs := decl.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			name := src[loc[2]:loc[3]]
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if name == "stopBeforeNewWork" {
				continue
			}
			if strings.Contains(src[loc[0]:end], "stopBeforeNewWork()") {
				sites[name] = true
			}
		}
	}
	return sites
}

// adaptersWithSuspendCheck returns the adapterDefs keys whose decoder is wrapped
// in withSuspendCheck, and the full set of keys, from wasm/adapter_metadata.go.
func adaptersWithSuspendCheck(t *testing.T) (wrapped, all map[string]bool) {
	t.Helper()
	src := repoFile(t, "wasm/adapter_metadata.go")
	key := regexp.MustCompile(`(?m)^\t"([A-Za-z]+)": \{`)
	locs := key.FindAllStringSubmatchIndex(src, -1)
	wrapped, all = map[string]bool{}, map[string]bool{}
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		all[name] = true
		if strings.Contains(src[loc[0]:end], "ResultStmts: withSuspendCheck(") {
			wrapped[name] = true
		}
	}
	return wrapped, all
}

// witCallOutcomeFuncs returns the WIT interface functions returning
// result<string, call-failure>, and the full set of function names.
// witCallOutcomeFuncs splits cleat.wit's functions three ways, not two.
//
// `outcome` returns result<string, call-failure>: a stop is a case of the type
// and a component guest cannot read it as anything else.
//
// `scalar` returns u64 or s64, wide enough to carry bit 31 with the sentinel's
// other 63 bits zero. A stop is expressible, but NOT structurally unmissable --
// the guest has to test for it, which is why a stopSurface naming a scalar WIT
// function must also name the Python methods that do (see the `py` field).
//
// s64 is included and u32 is not. Bit 31 is the sign bit of a 32-bit word, so
// on a u32 the sentinel is either unrepresentable or aliases a negative value
// depending on how the binding lifts it; on s64 it is an ordinary positive
// 2147483648. No stop site returns u32 today and this is what stops one being
// added quietly.
func witCallOutcomeFuncs(t *testing.T) (outcome, scalar, all map[string]bool) {
	t.Helper()
	src := repoFile(t, "python-sdk/wit/cleat.wit")
	fn := regexp.MustCompile(`(?s)\n\s*([a-z0-9-]+): func\(.*?\)\s*->\s*([^;]+);`)
	outcome, scalar, all = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range fn.FindAllStringSubmatch(src, -1) {
		name, ret := m[1], strings.TrimSpace(m[2])
		all[name] = true
		switch {
		case strings.Contains(ret, "call-failure"):
			outcome[name] = true
		case ret == "u64", ret == "s64":
			scalar[name] = true
		}
	}
	return outcome, scalar, all
}

// pySDKStopChecks reports which Python HostCalls methods test the stop sentinel
// before decoding, by parsing the SDK rather than trusting a list.
//
// "Tests it" means reaching _raise_if_stopped, directly or through
// _check_host_result, which calls it first. Both are in host_calls.py.
func pySDKStopChecks(t *testing.T) map[string]bool {
	t.Helper()
	src := repoFile(t, "python-sdk/cleat_sdk/host_calls.py")
	out := map[string]bool{}
	def := regexp.MustCompile(`(?m)^    def (\w+)\(`)
	locs := def.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := src[loc[0]:end]
		name := src[loc[2]:loc[3]]
		if strings.Contains(body, "_raise_if_stopped(") || strings.Contains(body, "_check_host_result(") {
			out[name] = true
		}
	}
	return out
}

func sortedStopKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheThreeStopSurfacesAgree is the guard. See the file comment for why the
// correspondence is three-way rather than two.
func TestTheThreeStopSurfacesAgree(t *testing.T) {
	sites := hostStopSites(t)
	wrappedAdapters, allAdapters := adaptersWithSuspendCheck(t)
	outcomeWit, scalarWit, allWit := witCallOutcomeFuncs(t)
	pyChecks := pySDKStopChecks(t)

	// -- vacuity, per thing rather than per total ---------------------------
	//
	// A floor on a total is satisfied by the wrong things being present: an
	// earlier guard in this repo passed `len(x) >= 8` while nine AssemblyScript
	// cases were invisible, because eight Java and Rust ones remained. So each
	// scan is checked for a specific member it cannot legitimately lose as well
	// as for a plausible size.
	for _, c := range []struct {
		what string
		got  map[string]bool
		min  int
		must string
	}{
		{"host stop sites", sites, 6, "DurableCall"},
		{"adapters with withSuspendCheck", wrappedAdapters, 6, "DurableCall"},
		{"all adapterDefs keys", allAdapters, 30, "DurableSleep"},
		{"WIT call-outcome functions", outcomeWit, 6, "durable-call"},
		{"WIT scalar-returning functions", scalarWit, 15, "durable-acquire-lock"},
		// The canary is deliberately NOT acquire_lock_ms, the method this file
		// actually requires. Using the method under test makes the vacuity check
		// fire first when it is removed, so the per-site check below can never
		// be reached and cannot be falsified. Measured: it did exactly that.
		{"Python methods testing the stop sentinel", pyChecks, 8, "signal_workflow"},
		{"all WIT functions", allWit, 30, "durable-sleep"},
	} {
		if len(c.got) < c.min {
			t.Fatalf("%s: found %d, expected at least %d -- this scan is matching "+
				"almost nothing and would pass whatever the tree said", c.what, len(c.got), c.min)
		}
		if !c.got[c.must] {
			t.Fatalf("%s: %q is missing, so the scan is reading the wrong thing. "+
				"A size floor alone would not have caught this: the other entries are present.",
				c.what, c.must)
		}
	}

	// -- 1. every host stop site declares its surfaces ----------------------
	for _, site := range sortedStopKeys(sites) {
		if _, ok := stopSurfaces[site]; !ok {
			t.Errorf("engine method %q consults stopBeforeNewWork and is not in stopSurfaces.\n\n"+
				"A new host stop site means a guest can now be refused on a call it may not "+
				"check. Add an entry naming its Go adapter and its WIT function, or an "+
				"exemption with a reason constant. Do not add an empty entry.", site)
		}
	}

	// -- 2. no stale entries ------------------------------------------------
	declared := map[string]bool{}
	for k := range stopSurfaces {
		declared[k] = true
	}
	for _, site := range sortedStopKeys(declared) {
		if !sites[site] {
			t.Errorf("stopSurfaces names %q, which no longer consults stopBeforeNewWork.\n\n"+
				"Either the guard was removed from the host -- which is the regression this "+
				"test exists for -- or the method was renamed and this entry was not.", site)
		}
	}

	// -- 3. and 4. each declared surface actually carries the check ---------
	for _, site := range sortedStopKeys(sites) {
		s, ok := stopSurfaces[site]
		if !ok {
			continue // already reported above
		}

		if len(s.adapters) == 0 {
			if s.adapterWhy == "" {
				t.Errorf("stopSurfaces[%q] names no Go adapter and gives no reason.\n\n"+
					"An empty list with no reason is indistinguishable from an entry "+
					"somebody has not finished.", site)
			}
		} else if s.adapterWhy != "" {
			t.Errorf("stopSurfaces[%q] names adapters %v AND carries the exemption %q.\n\n"+
				"The exemption is stale: the thing it excuses is present.", site, s.adapters, s.adapterWhy)
		}
		for _, a := range s.adapters {
			if !allAdapters[a] {
				t.Errorf("stopSurfaces[%q] names Go adapter %q, which is not in adapterDefs.", site, a)
				continue
			}
			if !wrappedAdapters[a] {
				t.Errorf("the host can refuse %s, but the Go adapter %q does not use "+
					"withSuspendCheck.\n\nA Go guest reads bit 31 through that decoder as an "+
					"ordinary field -- for the durable-call layout that is responseLen=0, "+
					"errCode=0: an empty SUCCESSFUL response. The guest then runs on and does "+
					"the work the segment exists to prevent. This is §3.83's defect, and #672 "+
					"measured that it presents as defers_run=0 with a suspension that looks "+
					"correct from outside.", site, a)
			}
		}

		if len(s.py) > 0 {
			allScalar := len(s.wit) > 0
			for _, w := range s.wit {
				if !scalarWit[w] {
					allScalar = false
				}
			}
			if !allScalar {
				t.Errorf("stopSurfaces[%q] names Python methods %v, but its WIT functions do "+
					"not all return scalars.\n\n`py` is the substitute for the result<...> "+
					"guarantee and means nothing where that guarantee holds. An entry carrying "+
					"both was probably copied from one that needed it.", site, s.py)
			}
		}

		if len(s.wit) == 0 {
			if s.witWhy == "" {
				t.Errorf("stopSurfaces[%q] names no WIT function and gives no reason.", site)
			}
		} else if s.witWhy != "" {
			t.Errorf("stopSurfaces[%q] names WIT functions %v AND carries the exemption %q.\n\n"+
				"The exemption is stale: the thing it excuses is present. If this is "+
				"%q, the signature has been redesigned and §3.110 can be closed.",
				site, s.wit, s.witWhy, reasonWitIsStillCoreABI)
		}
		for _, w := range s.wit {
			if !allWit[w] {
				t.Errorf("stopSurfaces[%q] names WIT function %q, which is not in cleat.wit.", site, w)
				continue
			}
			switch {
			case outcomeWit[w]:
				// A stop is a case of the return type. Nothing further to check:
				// the guest cannot receive it as anything else.
			case scalarWit[w]:
				// Expressible but missable, so the Python surface is required.
				// This is the branch §3.113 argued for and left unwritten.
				if len(s.py) == 0 {
					t.Errorf("the host can refuse %s, and WIT function %q returns a scalar "+
						"rather than result<string, call-failure>.\n\nThat is allowed -- bit 31 "+
						"fits -- but it gives up the guarantee the result type provided, so the "+
						"entry must name the Python SDK methods that test the sentinel. Add `py`.",
						site, w)
					continue
				}
				for _, meth := range s.py {
					if !pyChecks[meth] {
						t.Errorf("the host can refuse %s through the scalar-returning WIT "+
							"function %q, but the Python method %q does not reach "+
							"_raise_if_stopped.\n\nA component guest decodes the word's fields "+
							"and bit 31 reads as an ordinary value -- for the lock layout that is "+
							"errCode=0, acquired=false, an unremarkable \"someone else holds it\". "+
							"The workflow takes that branch and runs on. §3.202 is the same defect "+
							"on await_signals.", site, w, meth)
					}
				}
			default:
				t.Errorf("the host can refuse %s, but WIT function %q returns neither "+
					"result<string, call-failure> nor a 64-bit scalar.\n\nA component guest has "+
					"no case in which to receive the refusal and no free bit to carry it, so it "+
					"arrives as a value the service could also have produced -- the "+
					"forgeable-sentinel problem §3.83 and §3.110 both exist to remove.", site, w)
			}
		}
	}

	// -- 5. and 6. the reverse directions ----------------------------------
	//
	// Both are needed. A guard on a call the host never refuses is not free:
	// bit 31 is REACHABLE in one layout (packSleepResult, measured by
	// TestStopSentinelBitsAcrossEveryLayout), so a stray check there would read
	// an ordinary result as a stop. The forward direction cannot see that.
	declaredAdapters, declaredWit := map[string]bool{}, map[string]bool{}
	for _, s := range stopSurfaces {
		for _, a := range s.adapters {
			declaredAdapters[a] = true
		}
		for _, w := range s.wit {
			declaredWit[w] = true
		}
	}
	for _, a := range sortedStopKeys(wrappedAdapters) {
		if !declaredAdapters[a] {
			t.Errorf("Go adapter %q uses withSuspendCheck but no host stop site declares it.\n\n"+
				"Either a host guard was removed and this decoder now tests a bit the host "+
				"never sets, or the adapter is guarding a call that must NOT be guarded -- "+
				"see javaCallsThatMustNotCheck for the layout where bit 31 is reachable.", a)
		}
	}
	for _, w := range sortedStopKeys(outcomeWit) {
		if !declaredWit[w] {
			t.Errorf("WIT function %q returns result<string, call-failure> but no host stop "+
				"site declares it.\n\nThe type promises a refusal the host cannot produce.", w)
		}
	}

	t.Logf("stop surfaces in agreement: %d host sites, %d guarded Go adapters, %d WIT "+
		"call-outcome functions", len(sites), len(wrappedAdapters), len(outcomeWit))
}

// TestEveryResultReturningWitFunctionCanActuallyCarryTheStop closes a gap the
// three-way guard above cannot see.
//
// TestTheThreeStopSurfacesAgree checks the WIT *declaration*. It does not check
// the dispatcher that implements it, and those are different things: a function
// can be declared `result<string, call-failure>` while its dispatcher calls
// setResultString, which lifts the payload and throws the packed word's error
// and sentinel bits away. The declaration then promises a refusal the host
// cannot deliver.
//
// Measured 2026-09-04 while adding §3.300: reverting dispatchSideEffect to
// setResultString with the WIT left as result<...> failed NOTHING in the engine
// suite. The mismatch surfaces only when a real component guest makes that call,
// which no unit test does.
//
// Two hops, both parsed rather than declared, because both are already written
// down: the interface tables in component_callbacks.go map a WIT name to a
// cbType, and the switch in component_cgo.go maps a cbType to its dispatcher.
func TestEveryResultReturningWitFunctionCanActuallyCarryTheStop(t *testing.T) {
	outcomeWit, _, _ := witCallOutcomeFuncs(t)
	callbacks := repoFile(t, "engine/component_callbacks.go")
	cgo := repoFile(t, "engine/component_cgo.go")

	witToCb := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z0-9-]+)":\s*(cbType\w+),`).FindAllStringSubmatch(callbacks, -1) {
		witToCb[m[1]] = m[2]
	}
	cbToDispatch := map[string]string{}
	for _, m := range regexp.MustCompile(`case (cbType\w+):\s*\n\s*return entry\.backend\.(dispatch\w+)\(`).FindAllStringSubmatch(cgo, -1) {
		cbToDispatch[m[1]] = m[2]
	}

	// Vacuity, per thing: a scan that matched nothing would pass every case.
	if len(witToCb) < 30 || witToCb["durable-call"] == "" {
		t.Fatalf("wit->cbType scan found %d entries, durable-call=%q; it is reading the "+
			"wrong thing", len(witToCb), witToCb["durable-call"])
	}
	if len(cbToDispatch) < 30 || cbToDispatch["cbTypeDurableCallString"] == "" {
		t.Fatalf("cbType->dispatch scan found %d entries, cbTypeDurableCallString=%q; it is "+
			"reading the wrong thing", len(cbToDispatch), cbToDispatch["cbTypeDurableCallString"])
	}

	for _, w := range sortedStopKeys(outcomeWit) {
		cb, ok := witToCb[w]
		if !ok {
			t.Errorf("WIT function %q returns result<string, call-failure> but no interface "+
				"table in component_callbacks.go maps it to a cbType, so no dispatcher serves "+
				"it and a component guest calling it reaches nothing.", w)
			continue
		}
		fn, ok := cbToDispatch[cb]
		if !ok {
			t.Errorf("WIT function %q maps to %s, which the switch in component_cgo.go does "+
				"not dispatch.", w, cb)
			continue
		}
		body := funcBody(t, cgo+callbacks, fn)
		if strings.Contains(body, "setResultString(") {
			t.Errorf("%s serves %q, which is declared result<string, call-failure>, but it "+
				"calls setResultString.\n\nThat lifts the payload and discards the packed "+
				"word's error and sentinel bits, so the refusal the signature promises can "+
				"never arrive. Use setResultCallOutcome(decodeCallOutcome(...)), which tests "+
				"the sentinel by mask before reading any field.", fn, w)
		}
		if !strings.Contains(body, "setResultCallOutcome(") {
			t.Errorf("%s serves %q, declared result<string, call-failure>, but never calls "+
				"setResultCallOutcome, so it cannot produce the err case at all.", fn, w)
		}
	}
}

// funcBody returns the source of a Go function from its declaration to the
// closing brace at column 0. Fails rather than returning "" when absent: a
// lookup that finds nothing would make every assertion above pass vacuously.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func \([a-z]+ \*wasmtimeBackend\) ` + regexp.QuoteMeta(name) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no func %s in the component sources", name)
	}
	rest := src[loc[0]:]
	if end := regexp.MustCompile(`(?m)^\}`).FindStringIndex(rest); end != nil {
		return rest[:end[1]]
	}
	return rest
}
