package engine

import (
	"bytes"
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// wasiOfferedByTheExporter is the surface the stock wazero exporter binds,
// measured rather than listed. Both tests below derive from it, so neither can
// drift against the dependency.
func wasiOfferedByTheExporter(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	b := rt.NewHostModuleBuilder(wasi_snapshot_preview1.ModuleName)
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
	mod, err := b.Instantiate(ctx)
	if err != nil {
		t.Fatalf("instantiating the stock WASI surface: %v", err)
	}
	out := make([]string, 0, 46)
	for n := range mod.ExportedFunctionDefinitions() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// minimalGuestCalling builds, by hand, the smallest WebAssembly module that
// imports one wasi_snapshot_preview1 function and calls it: type, import,
// function, export and code sections, nothing else.
//
// Hand-built because the alternatives both measure the wrong thing. Calling the
// host module's export directly is not possible -- wazero panics with "cannot
// call host module functions directly", which is how this test first failed --
// and compiling a real guest would drag a language toolchain into a test about
// policy, and would only ever exercise the functions that toolchain happens to
// import.
//
// The import is declared (param i32 x nparams) (result i32), which is the shape
// of every WASI preview1 function used here. The exported "run" takes nothing,
// pushes nparams zeros and calls the import.
func minimalGuestCalling(name string, nparams int) []byte {
	return minimalGuestCallingWith(name, make([]int32, nparams), -1)
}

// minimalGuestCallingWith is minimalGuestCalling with the arguments spelled out
// and, when loadAddr >= 0, an i32 loaded from linear memory as the result
// instead of the call's own errno. That is how a test reads an OUT PARAMETER:
// environ_sizes_get and friends return an errno and write their answer through
// a pointer, so the interesting value is in memory rather than on the stack.
func minimalGuestCallingWith(name string, args []int32, loadAddr int32) []byte {
	u := func(n int) []byte { return []byte{byte(n)} } // every length here is < 128
	sect := func(id byte, body []byte) []byte {
		return append(append([]byte{id}, u(len(body))...), body...)
	}
	const i32 = 0x7F

	// type 0: the import's signature. type 1: () -> i32, for "run".
	nparams := len(args)
	t0 := append(append([]byte{0x60}, u(nparams)...), bytes.Repeat([]byte{i32}, nparams)...)
	t0 = append(t0, 0x01, i32)
	t1 := []byte{0x60, 0x00, 0x01, i32}
	types := sect(1, append(append(u(2), t0...), t1...))

	const modName = "wasi_snapshot_preview1"
	imp := append(u(len(modName)), modName...)
	imp = append(imp, u(len(name))...)
	imp = append(imp, name...)
	imp = append(imp, 0x00, 0x00) // kind: func, type index 0
	imports := sect(2, append(u(1), imp...))

	funcs := sect(3, []byte{0x01, 0x01}) // one function, type index 1

	// One page of memory, EXPORTED AS "memory". Not optional and not padding:
	// wasmtime's WASI implementation resolves the caller's memory export before
	// it does anything, so without this even an ALLOWED function fails with
	// "missing required memory export" -- which is a refusal that has nothing to
	// do with the policy. The negative control is what surfaced it; the refusal
	// assertions passed either way.
	memory := sect(5, []byte{0x01, 0x00, 0x01})
	exports := sect(7, []byte{
		0x02,
		0x03, 'r', 'u', 'n', 0x00, 0x01, // "run"    -> func 1
		0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00, // "memory" -> mem 0
	})

	body := []byte{0x00} // no locals
	for _, a := range args {
		body = append(body, 0x41, byte(a)) // i32.const a (all values here are < 64)
	}
	body = append(body, 0x10, 0x00) // call 0
	if loadAddr >= 0 {
		body = append(body,
			0x1A,                 // drop the errno
			0x41, byte(loadAddr), // i32.const loadAddr
			0x28, 0x02, 0x00) // i32.load align=4 offset=0
	}
	body = append(body, 0x0B) // end
	code := sect(10, append(u(1), append(u(len(body)), body...)...))

	out := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	for _, s := range [][]byte{types, imports, funcs, memory, exports, code} {
		out = append(out, s...)
	}
	return out
}

// TestTheWasiPolicyCoversEveryOfferedFunction is the artefact cleat#1381 asked
// for, and it is the point of the table rather than a check on it: the useful
// thing is not the refusals, it is something that fails when the OFFERED SET
// CHANGES. A dependency bump that adds a 47th function, or one that renames an
// existing one, lands here rather than silently inheriting a bucket.
//
// It compares the SETS and not the counts. Two derivations agreeing on 46 while
// differing on their membership is a documented failure in this repository --
// two export counts agreed at 55 while six names differed, three in each
// direction -- so a count assertion here would be the same mistake.
func TestTheWasiPolicyCoversEveryOfferedFunction(t *testing.T) {
	offered := wasiOfferedByTheExporter(t)
	if len(offered) == 0 {
		t.Fatal("the stock WASI exporter offered NOTHING, so this test compared two " +
			"empty sets and would agree with any policy table at all. That is a " +
			"broken measurement, not a clean tree.")
	}
	inPolicy := map[string]bool{}
	bucketed := make([]string, 0, len(wasiPreview1Policy))
	for n := range wasiPreview1Policy {
		inPolicy[n] = true
		bucketed = append(bucketed, n)
	}
	sort.Strings(bucketed)
	isOffered := map[string]bool{}
	for _, n := range offered {
		isOffered[n] = true
	}

	var unbucketed, phantom []string
	for _, n := range offered {
		if !inPolicy[n] {
			unbucketed = append(unbucketed, n)
		}
	}
	for _, n := range bucketed {
		if !isOffered[n] {
			phantom = append(phantom, n)
		}
	}

	if len(unbucketed) > 0 {
		t.Errorf("the host offers %d WASI function(s) that engine/wasi_policy.go does not "+
			"bucket: %v\n\n"+
			"Every offered function needs one of the three buckets cleat#1381 names -- "+
			"inherently deterministic, intercepted, or fatal. Until it has one, "+
			"wasiIsFatal refuses it by default, which is the safe direction but is not "+
			"a decision anybody made.",
			len(unbucketed), unbucketed)
	}
	if len(phantom) > 0 {
		t.Errorf("engine/wasi_policy.go buckets %d function(s) the host does not offer: %v\n\n"+
			"Either the dependency renamed or removed them, or the name is misspelled. "+
			"A misspelled key is the dangerous case: it looks like a considered decision "+
			"and refuses nothing, because wasiIsFatal keys on the real name.",
			len(phantom), phantom)
	}
	t.Logf("offered and bucketed: %d of %d", len(offered)-len(unbucketed), len(offered))
}

// TestTheRefusedWasiFunctionsActuallyTrapOnWazero is the known-positive. The
// test above can only say the table is internally consistent; it would pass
// unchanged if applyWasiPolicyWazero were deleted, because a table is not an
// enforcement. This one calls a refused function through the live runtime.
//
// It calls the function DIRECTLY on the instantiated host module rather than
// through a guest, deliberately: that is the same object a guest's import
// resolves to, and it removes the guest toolchain from a test about policy.
func TestTheRefusedWasiFunctionsActuallyTrapOnWazero(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	call := func(t *testing.T, name string, nparams int) error {
		t.Helper()
		mod, err := rt.wazeroRuntime.Instantiate(ctx, minimalGuestCalling(name, nparams))
		if err != nil {
			t.Fatalf("a guest importing %s did not even LINK: %v\n\n"+
				"That is the failure mode the derived signatures exist to prevent, and "+
				"it is worse than a wrong policy: every Go guest imports poll_oneoff "+
				"without calling it, so a link-time break refuses workflows that never "+
				"touch the function at all.", name, err)
		}
		defer mod.Close(ctx)
		_, err = mod.ExportedFunction("run").Call(ctx)
		return err
	}

	// fd_datasync is refused, and takes one i32.
	err = call(t, "fd_datasync", 1)
	if err == nil {
		t.Fatal("a guest CALLED fd_datasync and it returned normally. cleat#1381's " +
			"allowlist is not enforced on the wazero runtime: applyWasiPolicyWazero " +
			"either did not run, or its re-export lost to the stock exporter. The " +
			"order is silent when wrong -- the builder is last-wins, and a trap " +
			"registered BEFORE the exporter is discarded without an error.")
	}
	if !strings.Contains(err.Error(), "cleat refuses the WASI call") {
		t.Fatalf("fd_datasync failed, but not with cleat's refusal:\n  %v\n\n"+
			"Failing for the wrong reason is the hazard here. The refused families "+
			"ALREADY failed before this policy existed, for want of a preopened "+
			"directory -- containment by configuration. Reading that as enforcement "+
			"would leave the policy untested.", err)
	}

	// NEGATIVE CONTROL. Without it the assertion above is satisfied by a runtime
	// that refuses EVERYTHING, including the fifteen functions every Go guest
	// needs, and a suite that only ever asks "is it refused?" cannot tell an
	// allowlist from a wall.
	if err := call(t, "sched_yield", 0); err != nil {
		t.Fatalf("sched_yield is on the allowlist and was refused anyway: %v\n\n"+
			"The policy is refusing an ALLOWED function, so the refusal above says "+
			"nothing about fd_datasync in particular.", err)
	}
	t.Log("fd_datasync refused, sched_yield allowed")
}

// TestNoWasiFunctionARealGuestImportsIsRefused is the guard this policy needed
// and did not have, and it exists because the first version of the table was
// wrong in a way every other check here passed.
//
// poll_oneoff was bucketed fatal on the strength of a probe that counted its
// calls across three ordinary runs of testdata/basic -- a success, a guest
// error, and a long_running with three durable calls -- and found zero every
// time. The conclusion drawn was "every Go guest imports it without calling
// it". That was true of the runs measured and false of the function: the Go
// runtime reaches it when it parks a goroutine, which those three runs never
// did. An out-of-memory guest does, and refusing it turned a memory kill into
// a WASI refusal the host's OOM classification did not recognise, so a killed
// workflow was reported as SUCCEEDING.
//
// THE LESSON IS ABOUT THE SHAPE OF THE EVIDENCE, not about poll_oneoff. A
// census of calls on the happy path cannot see a path taken only when
// something is going wrong, and no amount of repeating it helps. What is
// checkable is the IMPORT, which is static, does not depend on reaching the
// condition, and is the conservative direction: a guest that imports a
// function may call it, and this test refuses to let the table bet otherwise.
func TestNoWasiFunctionARealGuestImportsIsRefused(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("reading the Go fixture: %v", err)
	}

	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("compiling the Go fixture: %v", err)
	}

	// DISTINCT names, not import entries. A Go guest declares fd_write twice --
	// 17 entries for 16 functions -- so a count of entries would disagree with
	// the census for a reason that has nothing to do with the policy.
	seen := map[string]bool{}
	var imported, refused []string
	for _, def := range compiled.ImportedFunctions() {
		mod, name, ok := def.Import()
		if !ok || mod != "wasi_snapshot_preview1" || seen[name] {
			continue
		}
		seen[name] = true
		imported = append(imported, name)
		if wasiIsFatal(name) {
			refused = append(refused, name)
		}
	}
	sort.Strings(imported)
	sort.Strings(refused)

	if len(imported) == 0 {
		t.Fatal("the Go fixture imports NO WASI functions, so this test compared an " +
			"empty set against the policy and would pass whatever the table said. " +
			"Either the fixture stopped being a wasip1 build or the import walk is " +
			"broken -- both are measurement failures, not a clean tree.")
	}
	if len(refused) > 0 {
		t.Errorf("a shipped Go guest imports %d WASI function(s) that engine/wasi_policy.go "+
			"refuses: %v\n\n"+
			"An import is not proof of a call, and it is deliberately treated as one "+
			"here. Whether a function is reached can depend on conditions a test suite "+
			"does not ordinarily produce -- memory pressure, a parked goroutine, a "+
			"failing allocation -- so 'I ran it and it was never called' is evidence "+
			"about the runs, not about the function. If one of these genuinely must be "+
			"refused, the case has to be made against the guest's runtime, not against "+
			"a call count.\n\n"+
			"all WASI imports in this fixture (%d): %v",
			len(refused), refused, len(imported), imported)
	}
	t.Logf("distinct WASI functions imported by the Go fixture: %d, none refused", len(imported))
}

// TestTheDeterministicBucketRestsOnAnEmptyConfiguration checks the assumption
// that four entries in the table are actually about, and cleat#1381 asked for
// this specifically: "becomes false the moment anything calls InheritEnv --
// worth a test, not just a note."
//
// args_*, environ_* and fd_read are bucketed deterministic BECAUSE CLEAT
// SUPPLIES NOTHING, which is a fact about the configuration and not about the
// functions. The path_* and sock_* refusals are the mirror image: before this
// policy existed, the only thing stopping path_open was that no directory is
// preopened. #1381 called that "containment by configuration, one line away
// from being lost" -- a preopen added for a plugin, or an InheritEnv added for
// configuration, would silently change what these functions do while the table
// above went on claiming they were deterministic.
//
// So this asserts the emptiness rather than the functions. If someone adds a
// preopen or an environment, this goes red and the table gets revisited, which
// is the whole point. It is not a claim that the change would be wrong.
func TestTheDeterministicBucketRestsOnAnEmptyConfiguration(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// THROUGH InstantiateModuleNamed, not through wazero's Instantiate. That is
	// load bearing and the difference is invisible in the result: a bare
	// Instantiate builds a DEFAULT ModuleConfig, so this test would be asserting
	// that wazero's defaults are empty -- true, unfalsifiable, and no statement
	// about cleat at all. It would stay green with WithEnv added to the config
	// the guests actually get, which is the single thing it exists to catch.
	call := func(t *testing.T, name string, args []int32, loadAddr int32) uint64 {
		t.Helper()
		compiled, err := rt.wazeroRuntime.CompileModule(ctx, minimalGuestCallingWith(name, args, loadAddr))
		if err != nil {
			t.Fatalf("compiling a guest importing %s: %v", name, err)
		}
		defer compiled.Close(ctx)
		mod, err := rt.InstantiateModuleNamed(ctx, compiled, "wasi-config-probe-"+name)
		if err != nil {
			t.Fatalf("a guest importing %s did not link: %v", name, err)
		}
		defer mod.Close(ctx)
		res, err := mod.ExportedFunction("run").Call(ctx)
		if err != nil {
			t.Fatalf("%s was refused or trapped, so this test measured nothing about "+
				"the configuration: %v", name, err)
		}
		return res[0]
	}

	if n := call(t, "environ_sizes_get", []int32{0, 8}, 0); n != 0 {
		t.Errorf("the guest sees %d environment variable(s), and engine/wasi_policy.go "+
			"buckets environ_get/environ_sizes_get as deterministic on the grounds that "+
			"cleat populates no environment.\n\n"+
			"Something now calls InheritEnv or its equivalent. The bucket is a statement "+
			"about the configuration, so it is the bucket that needs revisiting -- an "+
			"environment the host inherits is not replayable, and a workflow that reads "+
			"one produces a different answer on a different machine.", n)
	}
	if n := call(t, "args_sizes_get", []int32{0, 8}, 0); n != 0 {
		t.Errorf("the guest sees %d argv entr(ies); args_get/args_sizes_get are bucketed "+
			"deterministic because cleat passes none. Same reasoning as environ_*.", n)
	}

	// fd_prestat_get on the first non-standard descriptor. EBADF means there is
	// no preopened directory to enumerate, which is what makes the whole path_*
	// family unreachable even before it is refused.
	const wasiErrnoBadf = 8
	if e := call(t, "fd_prestat_get", []int32{3, 0}, -1); e != wasiErrnoBadf {
		t.Errorf("fd_prestat_get(3) returned errno %d, not EBADF (%d), so a directory is "+
			"preopened.\n\n"+
			"The path_* family is refused by this policy and was, before it, bounded only "+
			"by there being no descriptor to act on. A preopen restores the inputs to a "+
			"family of calls nobody has reasoned about; the refusals still hold, but the "+
			"reasoning in wasi_policy.go about what they cost is now wrong.", e, wasiErrnoBadf)
	}

	// fd_read against stdin. Bucketed deterministic BY ACCIDENT: no stdin is
	// configured, so it reports success having read nothing.
	if e := call(t, "fd_read", []int32{0, 16, 1, 0}, -1); e != 0 {
		t.Errorf("fd_read on stdin returned errno %d rather than succeeding at EOF. "+
			"fd_read is bucketed deterministic only because cleat configures no stdin; "+
			"if that changed, a workflow could read host input that no replay reproduces.", e)
	}
	t.Log("no environment, no argv, no preopened directory, no stdin")
}
