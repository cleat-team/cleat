package pluginharness

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEverySDKImportIsAHostExport asserts that every host function an SDK
// DECLARES an import for is a name the engine actually exports.
//
// This exists because two SDKs declared one that is not. The engine exports
// cleat_schedule_invoke; the Rust SDK declared `pub fn schedule_invoke` inside
// its `#[link(wasm_import_module = "env")]` block with no #[link_name], and the
// Java SDK declared `@Import(module = "env", name = "schedule_invoke")`. Neither
// runtime defines that name.
//
// A wrong import name does not fail at the call. It fails INSTANTIATION, so it
// takes the whole module with it, and it is invisible until something
// references the binding -- guest toolchains drop imports nothing calls. Proven
// by execution 2026-09-05, by adding one arm to the Rust host-call fixture:
//
//	before: engine: host: ... wasm trap: host: instantiate: unknown import:
//	        `env::schedule_invoke` has not been defined
//	after:  status=ok detail=scheduled
//
// So any Rust or Java workflow that so much as referenced scheduleInvoke could
// not run, and every compile-time coverage number counted the binding as
// present. That is the same shape as the Java arity defect in #760 and as
// IMPROVEMENT-PLAN 3.55 -- a declaration that disagrees with the host, where
// the disagreement is fatal and silent.
//
// sdkImportSource names one SDK's import surface and how to read it.
//
// Shared by both directions -- TestEverySDKImportIsAHostExport (an SDK must not
// import what the engine does not export) and TestEverySDKReachesEveryHostExport
// (the engine should not export what an SDK cannot reach). One set of
// extractors, so a fix to a parse improves both.
type sdkImportSource struct {
	name  string
	floor int
	fn    func(*testing.T, string) map[string]string // import name -> where
}

var sdkImportSources = []sdkImportSource{
	// Rust's floor is 44 where the others are 45, and the two are the
	// externs IMPROVEMENT-PLAN 3.220 removed: cleat_send_signal_and_wait
	// and cleat_reply_to_signal. Request/reply is now composed from
	// create_promise + signal_workflow + await_promise + resolve_promise,
	// so a Rust guest declares neither import.
	//
	// The floor exists to catch the EXTRACTOR breaking, not to freeze the
	// count, so it moves when imports are deliberately removed -- but only
	// after checking the drop is the removal and not a silently broken
	// parse. Measured 2026-09-06, extracting the extern block from each
	// revision: origin/develop 46, this branch 44.
	{"rust", 44, rustDeclaredImports},
	// Java's floor is 44 for the same reason as Rust's above: the two
	// @Import declarations IMPROVEMENT-PLAN 3.220 removed,
	// cleat_send_signal_and_wait and cleat_reply_to_signal. Measured
	// 2026-09-06 by diffing the SETS, not the counts -- origin/develop 46,
	// this branch 44, removed exactly those two and added none.
	{"java", 44, javaDeclaredImports},
	{"assemblyscript", 45, asDeclaredImports},

	// Go and Python declare nothing themselves, and are covered here by
	// the tables that decide their import names instead.
	//
	// A Go guest gets its imports from a generated adapter -- there is no
	// //go:wasmimport anywhere in the tree, only in docs -- and the name
	// comes from hostFunctions in wasm/usage.go. Python reaches the host
	// through the Component Model, so its names come from WitToEnvImport
	// in wasm/component_rewrite.go, which maps WIT (module, function)
	// pairs to flat "env" names.
	//
	// Both tables are host-side, so a mismatch is far less likely than in
	// an SDK that spells the name itself -- which is a real reason these
	// two are different in kind, not an excuse for skipping them. A typo
	// in either table is the same fatal-and-silent instantiation failure,
	// and neither had a check.
	{"go (wasm/usage.go hostFunctions)", 30, goAdapterImportNames},
	// Python's floor is 43 for the same reason as Rust's and Java's 44:
	// the two names IMPROVEMENT-PLAN 3.220 removed. Python is the only
	// SDK whose imports are decided host-side, by this table rather than
	// by a declaration in its own source, so the removal there IS the
	// removal here. Measured 2026-09-06 by diffing the SETS, not the
	// counts -- origin/develop 45, this branch 43, removed exactly
	// cleat_send_signal_and_wait and cleat_reply_to_signal, added none.
	{"python (wasm/component_rewrite.go WitToEnvImport)", 43, witEnvImportNames},
}

// Anchored on DECLARATION SITES, per CLAUDE.md: the extern block, the @Import
// annotation, the @external decorator. Not on names appearing in source, which
// cannot tell a declaration from a comment about one.
func TestEverySDKImportIsAHostExport(t *testing.T) {
	root := findProjectRoot(t)
	exports := hostExportNames(t, root)
	if len(exports) < 40 {
		t.Fatalf("found only %d .Export( registrations in engine/imports.go; "+
			"expected at least 40. If registration moved behind a helper, this "+
			"test compares against a set it never found.", len(exports))
	}

	for _, sdk := range sdkImportSources {
		t.Run(sdk.name, func(t *testing.T) {
			declared := sdk.fn(t, root)
			// Input assertion. An extractor that silently finds nothing
			// reports perfect agreement, which is the failure this whole
			// file is about.
			if len(declared) < sdk.floor {
				t.Fatalf("%s: found only %d import declarations, expected at least %d.\n\n"+
					"The extractor anchors on the declaration site. If the SDK changed how "+
					"it spells one, this test quietly stops checking most of them.",
					sdk.name, len(declared), sdk.floor)
			}

			var bad []string
			for name, where := range declared {
				if !exports[name] {
					bad = append(bad, name+" ("+where+")")
				}
			}
			sort.Strings(bad)
			if len(bad) > 0 {
				t.Errorf("%s declares %d import(s) the engine does not export: %s\n\n"+
					"A guest that references one of these fails INSTANTIATION -- not the "+
					"call, the whole module -- and only once something references it, "+
					"because unused imports are dropped by the toolchain. Fix the name in "+
					"the SDK; do not add the export.",
					sdk.name, len(bad), strings.Join(bad, ", "))
			}
		})
	}
}

func hostExportNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(root, "engine", "imports.go"))
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.Export\("([^"]+)"\)`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

// rustDeclaredImports reads the names inside the `extern "C"` block attributed
// with wasm_import_module = "env". A #[link_name] attribute overrides the Rust
// function name and IS the import name -- missing that is how this defect hid,
// since the two agree for every other declaration in the block.
func rustDeclaredImports(t *testing.T, root string) map[string]string {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(root, "crates", "cleat-sdk", "src", "host_calls.rs"))
	loc := regexp.MustCompile(`#\[link\(wasm_import_module = "env"\)\]\s*\n\s*extern "C" \{`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no `#[link(wasm_import_module = \"env\")] extern \"C\"` block in host_calls.rs.\n\n" +
			"If the SDK changed how it declares imports, teach this extractor the new " +
			"spelling rather than letting it find nothing.")
	}
	depth, i := 1, loc[1]
	for depth > 0 && i < len(src) {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
		i++
	}
	block := src[loc[1]:i]
	out := map[string]string{}
	re := regexp.MustCompile(`(?:#\[link_name = "([^"]+)"\]\s*\n\s*)?pub fn ([a-z_][a-z0-9_]*)\s*\(`)
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		out[name] = "crates/cleat-sdk/src/host_calls.rs"
	}
	return out
}

func javaDeclaredImports(t *testing.T, root string) map[string]string {
	t.Helper()
	p := filepath.Join(root, "crates", "cleat-java", "src", "main", "java", "cleat", "HostCalls.java")
	src := readFileOrFatal(t, p)
	out := map[string]string{}
	re := regexp.MustCompile(`@Import\(\s*module\s*=\s*"env"\s*,\s*name\s*=\s*"([^"]+)"\s*\)`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = "crates/cleat-java/.../HostCalls.java"
	}
	return out
}

func asDeclaredImports(t *testing.T, root string) map[string]string {
	t.Helper()
	p := filepath.Join(root, "packages", "cleat-as", "assembly", "host-calls.ts")
	src := readFileOrFatal(t, p)
	out := map[string]string{}
	re := regexp.MustCompile(`@external\("env",\s*"([^"]+)"\)`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = "packages/cleat-as/assembly/host-calls.ts"
	}
	return out
}

func readFileOrFatal(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// goAdapterImportNames reads the import names the generated Go adapter is
// built from. There is no //go:wasmimport in the tree; hostFunctions is where
// a Go guest's import name is decided.
func goAdapterImportNames(t *testing.T, root string) map[string]string {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(root, "wasm", "usage.go"))
	out := map[string]string{}
	// Positional struct literals: {"cleat_call", "DurableCall"}. Named fields
	// would not match, which is why the floor above is not decoration.
	//
	// The import name is optional in this pattern because a row MAY carry an
	// empty one, meaning the Go adapter for that method binds no host function
	// at all. Such a row is a real sentinel, not a typo, and is matched
	// deliberately and skipped below so that it is accounted for rather than
	// invisible to the row-count agreement check.
	//
	// There are none at present. RunDetached was the last, and it was a
	// sentinel with a cost: the method worked under localdev and cleattest and
	// silently did nothing in every compiled workflow. Its signature now
	// matches cleat_run_detached and the row carries a real import name.
	//
	// Note this scans the whole FILE, so a row literal written inside a comment
	// counts as a row and will disagree with the count taken from the slice.
	re := regexp.MustCompile(`\{"([a-z_][a-z0-9_]*)?",\s*"[A-Za-z0-9_]+"\}`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if m[1] == "" {
			continue
		}
		out[m[1]] = "wasm/usage.go"
	}

	// A FLOOR IS NOT ENOUGH HERE, and this is the second time today that
	// lesson has had to be learned: a floor answers "did the scan find
	// enough", never "did it find everything". Rewriting ONE row to named
	// fields makes the strict pattern miss it, and 34 of 35 clears any floor
	// worth setting -- so the guard silently stops requiring that name.
	//
	// So count the rows a LOOSER reading finds and require agreement. The
	// loose pattern would make a bad extractor: it takes anything that opens
	// a struct literal with a string. Its only job is to disagree.
	block := regexp.MustCompile(`(?s)var hostFunctions = \[\]HostFunction\{.*?\n\}`).FindString(src)
	if block == "" {
		t.Fatalf("could not find the hostFunctions slice literal in wasm/usage.go.\n\n" +
			"Teach this extractor the new shape rather than letting it check a subset.")
	}
	// Compare ROWS to ROWS. The table is deliberately many-to-one -- several
	// Go methods share one import -- so the count of distinct names is
	// legitimately smaller and comparing it here would fail on a healthy tree.
	// (It did, first try.)
	strictRows := len(re.FindAllString(block, -1))
	looseRows := len(regexp.MustCompile(`(?m)^\s*\{`).FindAllString(block, -1))
	if strictRows != looseRows {
		t.Errorf("the strict scan of hostFunctions matched %d rows and a looser "+
			"reading of the same block found %d.\n\n"+
			"Some row is spelled in a way the strict pattern does not match -- named "+
			"fields instead of positional, or a line break inside the literal. Every "+
			"row it cannot see is an import name this test cannot require. Teach the "+
			"pattern the new spelling; do not lower the floor.", strictRows, looseRows)
	}
	return out
}

// witEnvImportNames reads the flat "env" names the Component Model path
// rewrites WIT imports to -- the Python SDK's route to the host.
func witEnvImportNames(t *testing.T, root string) map[string]string {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(root, "wasm", "component_rewrite.go"))
	out := map[string]string{}
	re := regexp.MustCompile(`"[a-z0-9\-/:.]+"\s*:\s*"([a-z_][a-z0-9_]*)"`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = "wasm/component_rewrite.go"
	}
	return out
}

// notWorkflowFacing are host exports no SDK should ever bind, so their absence
// from every SDK is not a gap and must not be recorded as one.
//
// cleat_poll_work and cleat_complete are the worker handshake -- the runtime
// calls them, a guest never does. cleat_register_query_handler is deliberately
// unbindable: no engine version ever routed an external query to it, and every
// SDK carries a comment saying it is absent on purpose (see
// docs/determinism.md, "Why there is no RegisterQueryHandler").
var notWorkflowFacing = map[string]bool{
	"cleat_poll_work":              true,
	"cleat_complete":               true,
	"cleat_register_query_handler": true,
}

// sdkUnreachedBaseline records, per SDK, the workflow-facing host exports that
// SDK cannot reach. SHRINK-ONLY: removing an entry when the binding lands is
// the point; adding one widens the gap and needs a reason here.
//
// Measured 2026-09-07. Note that AssemblyScript, not Go, is the only SDK at
// full parity -- which is the sort of thing nobody would have guessed, and the
// reason this direction is worth checking at all.
var sdkUnreachedBaseline = map[string][]string{
	// rust and java: deliberately absent, as assemblyscript is. Both bound the
	// cron trio in 3.242 -- which is what this baseline existed to make
	// visible, and it worked on its first real use: applying the bindings made
	// this test fail with "names 3 host export(s) this SDK now reaches", naming
	// all three.

	// Go splits into two kinds, and calling all six "gaps" would overstate it.
	//
	// NOT gaps -- Go reaches the capability another way, and adding the host
	// call would be redundant:
	//   cleat_json_parse, cleat_json_stringify  encoding/json is in the standard
	//                                           library; the host call exists for
	//                                           guests whose language has no JSON.
	//   cleat_uuid                              composable and already durable as
	//                                           SideEffect(func() string {...}),
	//                                           which Go does bind
	//                                           (cleat_side_effect). A native
	//                                           uuid.New() would NOT be
	//                                           replay-safe; the host call is a
	//                                           convenience over the safe form,
	//                                           not the only safe form.
	//
	//   cleat_fetch                 Go DOES reach durable HTTP, by a different
	//                               route: DurableFetch/DurableFetchJSON/FetchGet/
	//                               FetchGetJSON all map to cleat_call
	//                               (wasm/usage.go:119-123, "all map to
	//                               durable_call import"), issuing
	//                               DurableCall("http", "fetch"). Both
	//                               ServiceCaller implementations intercept that
	//                               pair before any plugin lookup --
	//                               cmd/cleat-worker/setup.go:155 (the production
	//                               worker, with idempotency-key support) and
	//                               cleat/embedded/runner.go:394 -- and
	//                               cmd/cleat-worker/service_caller_errors_test.go
	//                               exercises it against a live httptest server.
	//                               So it is durable and replayable via the
	//                               cleat_call event, not the EventTypeFetch one.
	//
	//                               This entry read "a durable HTTP fetch. net/http
	//                               in a guest is not durable and not replayable"
	//                               until 2026-09-08. That sentence is true and was
	//                               the wrong question: it argues no NATIVE
	//                               substitute exists, which is right, and was filed
	//                               under a heading claiming no COMPOSED one does
	//                               either. Absence of an http plugin in plugins/
	//                               looks like confirmation and is not -- the
	//                               interception is in the ServiceCaller, above the
	//                               registry.
	//
	// cleat_get_scope / cleat_set_scope were the LAST real gap and are now
	// bound (cleat#984, 2026-09-09). They are gone from this list rather than
	// re-labelled, which is what shrink-only means. Settled the same way
	// IMPROVEMENT-PLAN 3.223 established the gap -- by building a Go workflow
	// and reading the binary, not by consulting the tables a fix edits:
	// TestACompiledGoWorkflowImportsTheScopeCalls compiles the fixture and
	// finds both imports, and
	// TestACompiledGoWorkflowActuallyReachesTheHostForScope asserts the
	// EventTypeScopeAcquired records only engine/scope.go can write.
	//
	// Go therefore has NO real gap left; the four below are all reachable
	// another way.
	"go (wasm/usage.go hostFunctions)": {
		"cleat_fetch",
		"cleat_json_parse",
		"cleat_json_stringify",
		"cleat_uuid",
	},

	// Python: five unreached, but only THREE are gaps.
	//
	// The first version of this comment said all five were real, "unlike Go's,
	// none of these has a native or composed substitute". That was wrong about
	// the json pair for exactly the reason it is wrong for Go: Python has the
	// `json` module -- host_calls.py imports it three times -- and
	// engine/lifecycle.go's JsonParse/JsonStringify are pure (unmarshal,
	// re-marshal, write; no recordEvent, no store), so a guest using its own
	// JSON diverges from nothing.
	//
	// Real gaps: await_any_child and poll_child are child-workflow control
	// flow, run_detached is a lifecycle primitive. Nothing composes those.
	//
	// Note this is the WIT rewrite table (host-side), not python-sdk's own
	// bindings. The two were compared on 2026-09-07 and agree on all 44 names
	// they share, differing only on cleat_register_query_handler, which is in
	// notWorkflowFacing above.
	"python (wasm/component_rewrite.go WitToEnvImport)": {

		// The json pair. Not gaps: the
		// `json` module is in the standard library and engine/lifecycle.go's
		// JsonParse/JsonStringify are pure -- unmarshal, re-marshal, write; no
		// recordEvent, no store -- so a guest using its own JSON diverges from
		// nothing. Same reasoning as Go's two, which sit in no baseline at all
		// because Go's row here does not exist.
		"cleat_json_parse",
		"cleat_json_stringify",
	},

	// assemblyscript: deliberately absent. It reaches every workflow-facing
	// export, and an empty entry here would read as "not yet measured".
}

// TestEverySDKReachesEveryHostExport is the reverse of
// TestEverySDKImportIsAHostExport, and it answers the question that one cannot.
//
// That test asks whether every name an SDK imports exists on the host. A
// mismatch there is fatal and loud: the guest fails to instantiate. This one
// asks whether every name the host offers is reachable from each SDK, and a
// mismatch is silent -- the capability simply does not exist in that language,
// and nothing anywhere says so.
//
// Both directions are needed because they fail differently, which is CLAUDE.md's
// "a check can tell you whether it is consistent with itself; it cannot tell you
// what it is not looking at". An SDK that binds NOTHING passes the forward test
// perfectly.
func TestEverySDKReachesEveryHostExport(t *testing.T) {
	root := findProjectRoot(t)
	exports := hostExportNames(t, root)
	if len(exports) < 40 {
		t.Fatalf("found only %d .Export( registrations in engine/imports.go", len(exports))
	}

	for _, sdk := range sdkImportSources {
		t.Run(sdk.name, func(t *testing.T) {
			declared := sdk.fn(t, root)
			if len(declared) < sdk.floor {
				t.Fatalf("%s: found only %d import declarations, expected at least %d -- "+
					"the extractor is broken, and a broken extractor makes THIS test "+
					"report the whole ABI as unreachable rather than reporting nothing.",
					sdk.name, len(declared), sdk.floor)
			}

			var unreached []string
			for name := range exports {
				if notWorkflowFacing[name] {
					continue
				}
				if _, ok := declared[name]; !ok {
					unreached = append(unreached, name)
				}
			}
			sort.Strings(unreached)

			base := sdkUnreachedBaseline[sdk.name]
			if widened := missingFrom(unreached, base); len(widened) > 0 {
				t.Errorf("%s cannot reach %d host export(s) beyond its recorded baseline:\n  %s\n\n"+
					"A host call added without a binding in this SDK widens the gap between "+
					"languages. Either bind it, or add it to sdkUnreachedBaseline[%q] with a "+
					"reason.", sdk.name, len(widened), strings.Join(widened, "\n  "), sdk.name)
			}
			if closed := missingFrom(base, unreached); len(closed) > 0 {
				t.Errorf("sdkUnreachedBaseline[%q] names %d host export(s) this SDK now reaches:\n  %s\n\n"+
					"Good news, and the baseline must shrink to match or it stops measuring "+
					"anything.", sdk.name, len(closed), strings.Join(closed, "\n  "))
			}
		})
	}
}

// missingFrom returns members of a that are not in b.
func missingFrom(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}
