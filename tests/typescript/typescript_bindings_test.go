package typescript

// The host-call binding inventory (cleat#3827).
//
// The issue's `Done when:` is that every host call a workflow can reach is either bound with a TypeScript signature or
// listed as deliberately unbound, with the list checked by a test so it cannot drift. This is that test. It replaces a
// paragraph of prose with a property, because an inventory in prose is a claim nothing checks: the previous count of
// bound calls was right on the day it was written and would have gone stale silently the first time one was added.
//
// FOUR SOURCES, DELIBERATELY INDEPENDENT. The point is not that each is checked, it is that they can disagree:
//
//	engine/imports.go                        what the engine exports (55 today)
//	crates/cleat-js-plugin/src/**/*.rs       what the plugin actually installs (g.set("__cleat_…")): lib.rs and src/groups/
//	packages/cleat-ts/index.ts, groups/*.ts  what TypeScript is told exists (declare function __cleat_…)
//	packages/cleat-ts/host-calls.json and host-calls.d/*.json
//	                                         the decided mapping, plus every unbound call with its reason
//
// THE INVENTORY AND THE TWO SOURCES THAT DESCRIBE THE GUEST ARE SPLIT BY GROUP (cleat#3827), so that binding calls in two
// groups edits different files. The reconciler below still sees ONE inventory, the union, and the union is where a mistake
// the split could introduce is caught: a call listed in two files, a group with a file missing, a file for a group nobody
// pinned. See packages/cleat-ts/groups/README.md.
//
// A single source reconciled against itself cannot report what it does not mention. Each pair below catches a different
// mistake: an export missing from the inventory (forgotten), a global installed but unmapped (undocumented), a mapping
// for a call the plugin does not install (a phantom binding), a binding TypeScript cannot see (invisible to users), and
// an unbound entry whose call is now bound (a list that did not shrink when the work landed).
//
// THE COUNT IS NOT THE GATE. Exact set equality is, so a new export forces a decision rather than changing a total, and
// binding a call fails until its allowlist entry is removed. No numbers are pinned anywhere in this file.
//
// The sources are read as TEXT and that is a known limitation: a scan of source cannot tell a call from a sentence about
// one, so comments are stripped by a small lexer before anything is matched. It is not a substitute for running the
// thing — the behavioural half (a bound global is actually callable in a guest) needs a built module and belongs with
// the binding tests that have one.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// bindingInventory is packages/cleat-ts/host-calls.json.
type bindingInventory struct {
	Doc      []string          `json:"doc"`      // the file's own explanation, for a reader of the JSON alone
	Plumbing []string          `json:"plumbing"` // __cleat_* globals that are ABI bridge, not host calls
	Bound    map[string]string `json:"bound"`    // __cleat_* global -> ABI export name
	Unbound  map[string]string `json:"unbound"`  // ABI export name -> "pending: …" or "permanent: …"

	// Problems are what building the union found wrong with the FILES, before any rule about their contents: a call
	// listed in two of them, a group with a file missing, a file for a group that is not pinned. They are carried here,
	// not returned as an error, so reconcileBindings reports them beside every other violation and the self-test below
	// can feed it a case.
	Problems []string `json:"-"`
}

// bindingGroups are the groups packages/cleat-ts/groups/README.md names, pinned so that a file that goes missing, or a
// misspelt one that would otherwise read as a new group, fails instead of being skipped. Adding a group means adding its
// four files and its name here.
var bindingGroups = []string{"updates", "signals", "children", "detached", "locks", "schedules", "continuation", "calls", "plugins", "utilities"}

// groupInventory is one packages/cleat-ts/host-calls.d/<group>.json.
type groupInventory struct {
	Doc     []string          `json:"doc"`
	Bound   map[string]string `json:"bound"`
	Unbound map[string]string `json:"unbound"`
}

// inventoryPart is one parsed group file and the name it is reported under.
type inventoryPart struct {
	name string
	inv  groupInventory
}

// mergeInventories builds the union of the main inventory and the group files. A call that appears in two files is a
// PROBLEM, recorded rather than resolved: merging two maps would otherwise collapse a duplicate into one key and report
// nothing, which is exactly the mistake the split makes possible (two groups each deciding they own a call). The main
// file is not modified.
func mergeInventories(main bindingInventory, parts []inventoryPart) bindingInventory {
	out := bindingInventory{
		Doc:      main.Doc,
		Plumbing: main.Plumbing,
		Bound:    map[string]string{},
		Unbound:  map[string]string{},
		Problems: append([]string(nil), main.Problems...),
	}
	boundIn := map[string]string{}   // global -> the file that binds it
	unboundIn := map[string]string{} // export -> the file that lists it
	const mainName = "host-calls.json"
	for g, e := range main.Bound {
		out.Bound[g], boundIn[g] = e, mainName
	}
	for e, r := range main.Unbound {
		out.Unbound[e], unboundIn[e] = r, mainName
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].name < parts[j].name })
	for _, p := range parts {
		file := "host-calls.d/" + p.name + ".json"
		for _, g := range sortedKeys(p.inv.Bound) {
			if prev, dup := boundIn[g]; dup {
				out.Problems = append(out.Problems, fmt.Sprintf("%s and %s both bind %s: a call belongs to one group, and one file lists it", prev, file, g))
				continue
			}
			out.Bound[g], boundIn[g] = p.inv.Bound[g], file
		}
		for _, e := range sortedKeys(p.inv.Unbound) {
			if prev, dup := unboundIn[e]; dup {
				out.Problems = append(out.Problems, fmt.Sprintf("%s and %s both list %s as unbound: a call belongs to one group, and one file lists it", prev, file, e))
				continue
			}
			out.Unbound[e], unboundIn[e] = p.inv.Unbound[e], file
		}
	}
	return out
}

// bindingSources is everything reconcileBindings compares. It is a value so the self-test below can feed it synthetic
// inputs: a reconciler with no known-positive is a claim that it works, and every surviving bug in this file's family
// was a check that passed a tree it should have failed.
type bindingSources struct {
	exported  []string // engine/imports.go
	installed []string // crates/cleat-js-plugin/src/lib.rs
	declared  []string // packages/cleat-ts/index.ts
	inventory bindingInventory
	// nameExceptions is the pinned, deliberately-odd part of the global -> export naming rule. It is an input rather
	// than a package-level constant so this value stays a function of everything it reads.
	nameExceptions map[string]string
}

// handshakeExports are the two worker handshake calls, which no guest can reach, and the one export ABI deliberately
// makes unbindable. They must stay listed as "permanent": if one is ever bound, that is a change to the ABI contract and
// worth failing on rather than quietly allowing.
var handshakeExports = []string{"cleat_poll_work", "cleat_complete", "cleat_register_query_handler"}

var bindingReasonPrefix = regexp.MustCompile(`^(pending|permanent): \S.{15,}$`)

// bindingNameExceptions pins a global whose export name cannot be derived from the global's, and the reason is a fact
// about the ABI rather than a convenience: three exports carry no cleat_ prefix, and a workflow-callable one is bound.
// Every OTHER global must map to `cleat_` + its own suffix.
//
// This closes the hole cleat-review found on #3908: the mapping's VALUE was checked only for "is a real export", so
// `__cleat_uuid` -> `cleat_random` with `cleat_uuid` moved to unbound satisfied every other rule while uuid() returned a
// random float. A naming rule is a proxy for the call site inside the closure; a full check would parse what each
// `g.set` actually calls, and the Rust SDK's #[link_name] renames are why that parse is not one line.
var bindingNameExceptions = map[string]string{"__cleat_set_query_state": "set_query_state"}

// reconcileBindings returns one message per violation, in a stable order. Empty means the inventory and the three
// sources agree.
func reconcileBindings(s bindingSources) []string {
	v := append([]string(nil), s.inventory.Problems...)
	exported := toSet(s.exported)
	installed := toSet(s.installed)
	declared := toSet(s.declared)

	boundGlobals := sortedKeys(s.inventory.Bound)
	plumbing := toSet(s.inventory.Plumbing)

	// A global is ABI plumbing or a host call, never both. Without this, listing a binding in `plumbing` would satisfy
	// the install rule and exempt it from the "declared in index.ts" rule, while it still counted as bound.
	for _, g := range sortedKeys(plumbing) {
		if _, ok := s.inventory.Bound[g]; ok {
			v = append(v, fmt.Sprintf("host-calls.json lists %s as both plumbing and a binding; a host call is one or the other", g))
		}
	}

	boundExports := map[string]string{} // export -> the global that binds it
	for _, g := range boundGlobals {
		name := s.inventory.Bound[g]
		if prev, dup := boundExports[name]; dup {
			v = append(v, fmt.Sprintf("host-calls.json: the export %s is bound twice, by %s and %s; one export has one binding", name, prev, g))
		}
		boundExports[name] = g
		if !strings.HasPrefix(g, "__cleat_") {
			v = append(v, fmt.Sprintf("host-calls.json: %q is not a __cleat_* global name", g))
		}
		if _, ok := exported[name]; !ok {
			v = append(v, fmt.Sprintf("host-calls.json: bound global %s names the export %q, which engine/imports.go does not export", g, name))
		}
		want, excepted := s.nameExceptions[g]
		if !excepted {
			want = "cleat_" + strings.TrimPrefix(g, "__cleat_")
		}
		if name != want {
			v = append(v, fmt.Sprintf("host-calls.json maps %s to %q, want %q: a swapped mapping satisfies every other rule here", g, name, want))
		}
	}
	// A pin for a global that is no longer bound is stale, and would silently stop guarding anything.
	for _, g := range sortedKeys(s.nameExceptions) {
		if _, ok := s.inventory.Bound[g]; !ok {
			v = append(v, fmt.Sprintf("bindingNameExceptions pins %s, which host-calls.json no longer binds", g))
		}
	}

	// The plugin installs exactly the plumbing plus the bound globals. Installed-but-unmapped would be a host call no one
	// can find in the inventory; mapped-but-not-installed is a phantom binding TypeScript would call into a TypeError.
	for _, g := range sortedKeys(installed) {
		if _, ok := plumbing[g]; ok {
			continue
		}
		if _, ok := s.inventory.Bound[g]; ok {
			continue
		}
		v = append(v, fmt.Sprintf("crates/cleat-js-plugin/src installs %s, but host-calls.json and host-calls.d/ list it as neither plumbing nor bound", g))
	}
	for _, g := range append(sortedKeys(plumbing), boundGlobals...) {
		if _, ok := installed[g]; !ok {
			v = append(v, fmt.Sprintf("host-calls.json lists %s, but the plugin never installs it (no g.set(\"%s\", …) under crates/cleat-js-plugin/src)", g, g))
		}
	}

	// TypeScript declares everything it can call. A bound global that index.ts does not declare is a host call with no
	// TypeScript signature, which is half of what the issue asks for. Plumbing may go undeclared: index.ts reads
	// __cleat_args/result/fail/suspended, while __cleat_shims_off is read by shims.js, not by the SDK.
	for _, g := range boundGlobals {
		if _, ok := declared[g]; !ok {
			v = append(v, fmt.Sprintf("%s is bound but packages/cleat-ts (index.ts or groups/) does not declare it, so no TypeScript signature exists", g))
		}
	}
	for _, g := range sortedKeys(declared) {
		if _, ok := installed[g]; !ok {
			v = append(v, fmt.Sprintf("packages/cleat-ts declares %s, which the plugin never installs: a call to it fails with \"not a function\"", g))
		}
	}

	// Exact set equality, both directions. The issue's Done when:, as a predicate.
	for _, name := range sortedKeys(exported) {
		_, bound := boundExports[name]
		reason, listed := s.inventory.Unbound[name]
		switch {
		case bound && listed:
			v = append(v, fmt.Sprintf("%s is both bound (%s) and listed as unbound; binding a call means removing its entry from the unbound list", name, boundExports[name]))
		case !bound && !listed:
			v = append(v, fmt.Sprintf("engine/imports.go exports %s, which host-calls.json neither binds nor lists as unbound: decide which and say why", name))
		case listed && reason == "":
			v = append(v, fmt.Sprintf("the inventory lists %s as unbound with no reason", name))
		case listed && !bindingReasonPrefix.MatchString(reason):
			v = append(v, fmt.Sprintf("the inventory's reason for %s does not start \"pending: \" or \"permanent: \" with an explanation: %q", name, reason))
		}
	}
	for _, name := range sortedKeys(s.inventory.Unbound) {
		if _, ok := exported[name]; !ok {
			v = append(v, fmt.Sprintf("host-calls.json lists %s as unbound, but engine/imports.go does not export it (renamed or removed?)", name))
		}
	}

	// The three that must stay unbindable, pinned by name rather than by a count.
	for _, name := range handshakeExports {
		if strings.HasPrefix(s.inventory.Unbound[name], "permanent: ") {
			continue
		}
		v = append(v, fmt.Sprintf("%s must stay listed as \"permanent: …\" unbound: it is not guest-reachable by design", name))
	}
	return v
}

func TestEveryHostCallIsBoundOrListed(t *testing.T) {
	root := repoRoot(t)
	s := bindingSources{
		exported:       engineExports(t, root),
		installed:      pluginGlobals(t, root),
		declared:       declaredGlobals(t, root),
		inventory:      loadInventory(t, root),
		nameExceptions: bindingNameExceptions,
	}
	if len(s.exported) == 0 || len(s.installed) == 0 {
		// A scan that returns nothing must not read as agreement: empty sources would satisfy several of the rules above.
		t.Fatalf("a source parsed to nothing (exports=%d installed=%d); the scan is broken, not the tree", len(s.exported), len(s.installed))
	}
	if v := reconcileBindings(s); len(v) > 0 {
		sort.Strings(v)
		t.Fatalf("the host-call binding inventory disagrees with the tree in %d place(s):\n  %s", len(v), strings.Join(v, "\n  "))
	}
	t.Logf("%d host calls: %d bound, %d listed unbound (of which %d permanent)", len(s.exported), len(s.inventory.Bound), len(s.inventory.Unbound), len(handshakeExports))
}

// TestReconcileBindingsSelfTest makes the reconciler report cases that are ALREADY broken. Without it, every rule above
// is a claim: a reconciler whose rules silently never fire passes any tree, and the failure it exists to catch is an
// unbound call nobody notices.
func TestReconcileBindingsSelfTest(t *testing.T) {
	clean := func() bindingSources {
		return bindingSources{
			exported:  []string{"cleat_call", "cleat_sleep", "cleat_later", "cleat_poll_work", "cleat_complete", "cleat_register_query_handler"},
			installed: []string{"__cleat_args", "__cleat_call", "__cleat_sleep"},
			declared:  []string{"__cleat_args", "__cleat_call", "__cleat_sleep"},
			inventory: bindingInventory{
				Plumbing: []string{"__cleat_args"},
				Bound:    map[string]string{"__cleat_call": "cleat_call", "__cleat_sleep": "cleat_sleep"},
				Unbound: map[string]string{
					"cleat_later":                  "pending: this one is not bound yet.",
					"cleat_poll_work":              "permanent: the worker's handshake, not a guest call.",
					"cleat_complete":               "permanent: the worker's handshake, not a guest call.",
					"cleat_register_query_handler": "permanent: deliberately unbindable from a guest.",
				},
			},
		}
	}
	if v := reconcileBindings(clean()); len(v) > 0 {
		t.Fatalf("the clean fixture must reconcile to nothing, got:\n  %s", strings.Join(v, "\n  "))
	}

	cases := []struct {
		name string
		want string // a substring of the violation that must appear
		mut  func(*bindingSources)
	}{
		{"an export in neither list", "neither binds nor lists", func(s *bindingSources) {
			s.exported = append(s.exported, "cleat_new")
		}},
		{"an export both bound and unbound", "both bound", func(s *bindingSources) {
			s.inventory.Unbound["cleat_sleep"] = "pending: this should have been removed when it was bound."
		}},
		{"an unbound entry that is not an export", "does not export it", func(s *bindingSources) {
			s.inventory.Unbound["cleat_gone"] = "pending: a name the engine no longer exports."
		}},
		{"a bound name that is not an export", "which engine/imports.go does not export", func(s *bindingSources) {
			s.inventory.Bound["__cleat_ghost"] = "cleat_ghost"
			s.installed = append(s.installed, "__cleat_ghost")
			s.declared = append(s.declared, "__cleat_ghost")
		}},
		{"a global installed but unmapped", "list it as neither plumbing nor bound", func(s *bindingSources) {
			s.installed = append(s.installed, "__cleat_extra")
		}},
		{"a mapping the plugin never installs", "the plugin never installs it", func(s *bindingSources) {
			// The mapping stays; the global goes. (Removing the mapping instead is the opposite defect, covered above.)
			s.installed = []string{"__cleat_args", "__cleat_call"}
			s.declared = []string{"__cleat_args", "__cleat_call"}
		}},
		{"a binding TypeScript cannot see", "does not declare it", func(s *bindingSources) {
			s.declared = []string{"__cleat_args", "__cleat_call"}
		}},
		{"a declaration the plugin never installs", "fails with \"not a function\"", func(s *bindingSources) {
			s.declared = append(s.declared, "__cleat_imaginary")
		}},
		{"an unbound entry with no reason", "with no reason", func(s *bindingSources) {
			s.inventory.Unbound["cleat_later"] = ""
		}},
		{"an unbound entry with a reason that is not a decision", "does not start", func(s *bindingSources) {
			s.inventory.Unbound["cleat_later"] = "TODO"
		}},
		{"a mapping that names the wrong export", "a swapped mapping satisfies every other rule", func(s *bindingSources) {
			// cleat-review's reconstruction on #3908: swap uuid's mapping and move cleat_uuid to unbound.
			s.inventory.Bound["__cleat_uuid"] = "cleat_random"
			delete(s.inventory.Unbound, "cleat_uuid")
			s.inventory.Unbound["cleat_uuid"] = "pending: moved here by the swap."
		}},
		{"a pin for a global that is no longer bound", "no longer binds", func(s *bindingSources) {
			s.nameExceptions = map[string]string{"__cleat_now": "cleat_now_renamed"}
		}},
		{"a global listed as both plumbing and a binding", "a host call is one or the other", func(s *bindingSources) {
			s.inventory.Plumbing = append(s.inventory.Plumbing, "__cleat_sleep")
		}},
		{"a handshake call no longer marked permanent", "must stay listed", func(s *bindingSources) {
			s.inventory.Unbound["cleat_poll_work"] = "pending: maybe a workflow could poll its own queue?"
		}},
		// The split's own failure modes. These go through mergeInventories, because a duplicate that has been merged into one
		// map key is invisible to every rule above.
		{"a call listed unbound in two group files", "both list cleat_later as unbound", func(s *bindingSources) {
			s.inventory = mergeInventories(bindingInventory{Bound: s.inventory.Bound, Plumbing: s.inventory.Plumbing, Unbound: map[string]string{
				"cleat_poll_work":              s.inventory.Unbound["cleat_poll_work"],
				"cleat_complete":               s.inventory.Unbound["cleat_complete"],
				"cleat_register_query_handler": s.inventory.Unbound["cleat_register_query_handler"],
			}}, []inventoryPart{
				{"locks", groupInventory{Unbound: map[string]string{"cleat_later": "pending: listed by the locks group."}}},
				{"signals", groupInventory{Unbound: map[string]string{"cleat_later": "pending: and also by the signals group."}}},
			})
		}},
		{"a call bound by two group files", "both bind __cleat_sleep", func(s *bindingSources) {
			s.inventory = mergeInventories(s.inventory, []inventoryPart{
				{"locks", groupInventory{Bound: map[string]string{"__cleat_sleep": "cleat_sleep"}}},
			})
		}},
		{"a call bound in one file and listed unbound in another", "both bound", func(s *bindingSources) {
			s.inventory = mergeInventories(s.inventory, []inventoryPart{
				{"locks", groupInventory{Unbound: map[string]string{"cleat_sleep": "pending: this group forgot it was bound in the main file."}}},
			})
		}},
		{"a group file that is missing", "has no host-calls.d/ghost.json", func(s *bindingSources) {
			s.inventory.Problems = append(s.inventory.Problems, groupFileProblems("ghost", false, true, true)...)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := clean()
			c.mut(&s)
			v := reconcileBindings(s)
			if len(v) == 0 {
				t.Fatalf("the reconciler accepted a fixture with a known defect (%s): it cannot report the case it exists for", c.name)
			}
			for _, msg := range v {
				if strings.Contains(msg, c.want) {
					return
				}
			}
			t.Fatalf("the reconciler refused the fixture for the wrong reason: want a violation containing %q, got:\n  %s", c.want, strings.Join(v, "\n  "))
		})
	}
}

// ---- the four sources -------------------------------------------------------------------------------------------------

// loadInventory reads packages/cleat-ts/host-calls.json and every packages/cleat-ts/host-calls.d/<group>.json and returns
// their union. Structural problems with the files are carried in Problems (see mergeInventories).
func loadInventory(t *testing.T, root string) bindingInventory {
	t.Helper()
	dir := filepath.Join(root, "packages", "cleat-ts")
	path := filepath.Join(dir, "host-calls.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the binding inventory: %v", err)
	}
	var inv bindingInventory
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // a misspelt key ("unbounded", "binding") would otherwise leave a map empty and pass
	if err := dec.Decode(&inv); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	known := toSet(bindingGroups)
	var parts []inventoryPart
	entries, err := os.ReadDir(filepath.Join(dir, "host-calls.d"))
	if err != nil {
		// Not a skip: with the directory gone the union is the main file alone, which would read as "every pending call
		// was bound" and pass the set-equality rule only if the engine's exports also disappeared. Say what is wrong.
		t.Fatalf("reading packages/cleat-ts/host-calls.d: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			inv.Problems = append(inv.Problems, fmt.Sprintf("packages/cleat-ts/host-calls.d/%s is not a <group>.json inventory file", name))
			continue
		}
		group := strings.TrimSuffix(name, ".json")
		present[group] = true
		if !known[group] {
			inv.Problems = append(inv.Problems, fmt.Sprintf("packages/cleat-ts/host-calls.d/%s is a file for %q, which is not one of the pinned groups: add it to bindingGroups and give it groups/%s.ts and src/groups/%s.rs, or fix the name", name, group, group, group))
		}
		raw, err := os.ReadFile(filepath.Join(dir, "host-calls.d", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		var g groupInventory
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if err := d.Decode(&g); err != nil {
			t.Fatalf("parsing host-calls.d/%s: %v", name, err)
		}
		parts = append(parts, inventoryPart{name: group, inv: g})
	}
	for _, g := range bindingGroups {
		_, tsErr := os.Stat(filepath.Join(dir, "groups", g+".ts"))
		_, rsErr := os.Stat(filepath.Join(root, "crates", "cleat-js-plugin", "src", "groups", g+".rs"))
		inv.Problems = append(inv.Problems, groupFileProblems(g, present[g], tsErr == nil, rsErr == nil)...)
	}
	return mergeInventories(inv, parts)
}

// groupFileProblems names each of a group's three files that is missing. A group with a file gone would otherwise lose its
// stub silently: the inventory would still reconcile, and the group's author would find out when their PR did not build.
func groupFileProblems(group string, hasJSON, hasTS, hasRS bool) []string {
	var v []string
	if !hasJSON {
		v = append(v, fmt.Sprintf("group %q has no host-calls.d/%s.json", group, group))
	}
	if !hasTS {
		v = append(v, fmt.Sprintf("group %q has no packages/cleat-ts/groups/%s.ts", group, group))
	}
	if !hasRS {
		v = append(v, fmt.Sprintf("group %q has no crates/cleat-js-plugin/src/groups/%s.rs", group, group))
	}
	return v
}

var (
	exportCallRE    = regexp.MustCompile(`\.Export\("([A-Za-z0-9_]+)"\)`)
	pluginSetRE     = regexp.MustCompile(`g\.set\(\s*"(__cleat_[A-Za-z0-9_]+)"`)
	tsDeclarationRE = regexp.MustCompile(`declare function (__cleat_[A-Za-z0-9_]+)\s*\(`)
)

func engineExports(t *testing.T, root string) []string {
	t.Helper()
	return scanFile(t, filepath.Join(root, "engine", "imports.go"), exportCallRE)
}

// pluginGlobals scans every .rs file under the plugin's src/, not lib.rs alone: the calls bound by cleat#3827 are installed
// from src/groups/. A scan of lib.rs only would report every one of them as a phantom binding.
func pluginGlobals(t *testing.T, root string) []string {
	t.Helper()
	return scanFiles(t, walkFiles(t, filepath.Join(root, "crates", "cleat-js-plugin", "src"), ".rs"), pluginSetRE)
}

// declaredGlobals scans index.ts and every packages/cleat-ts/groups/*.ts, for the same reason.
func declaredGlobals(t *testing.T, root string) []string {
	t.Helper()
	files := append([]string{filepath.Join(root, "packages", "cleat-ts", "index.ts")}, walkFiles(t, filepath.Join(root, "packages", "cleat-ts", "groups"), ".ts")...)
	return scanFiles(t, files, tsDeclarationRE)
}

// walkFiles returns every file under dir with the given suffix, sorted. A directory that holds none is a failure, not an
// empty answer: "no bindings found" would satisfy several of the reconciler's rules.
func walkFiles(t *testing.T, dir, suffix string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, suffix) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no %s files under %s: the scan is looking in the wrong place", suffix, dir)
	}
	sort.Strings(out)
	return out
}

// scanFiles is scanFile over several files, with one result set.
func scanFiles(t *testing.T, paths []string, re *regexp.Regexp) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		for _, name := range scanFile(t, p, re) {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func scanFile(t *testing.T, path string, re *regexp.Regexp) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(stripComments(string(b)), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// stripComments blanks out `//` and `/* … */` comments while leaving string literals intact, so a comment that quotes a
// call — lib.rs's header quotes `__cleat_args()` and `__cleat_result(json)`, and this file's own names appear in prose —
// cannot be read as the call itself. String literals are tracked so a `//` inside one is not mistaken for a comment.
// It models the literals these three files use (", ' and the backtick raw string / template); a regular-expression
// literal is not modelled, and the only one in them contains no `//` or `/*`.
func stripComments(src string) string {
	out := []byte(src)
	const (
		code = iota
		lineComment
		blockComment
		inString
	)
	state := code
	var quote byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state, out[i], out[i+1] = lineComment, ' ', ' '
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state, out[i], out[i+1] = blockComment, ' ', ' '
				i++
			case c == '"' || c == '\'' || c == '`':
				state, quote = inString, c
			}
		case lineComment:
			if c == '\n' {
				state = code
				continue
			}
			out[i] = ' '
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = code
				continue
			}
			if c != '\n' {
				out[i] = ' '
			}
		case inString:
			if c == '\\' && quote != '`' {
				i++ // the escaped byte is part of the string; leave both
				continue
			}
			if c == quote {
				state = code
			}
		}
	}
	return string(out)
}

// TestStripCommentsKeepsCallsAndDropsProse pins the one thing the three scans depend on. Both lib.rs and index.ts are
// full of prose that names a call (`__cleat_args()`, `__cleat_result(json)`), and a scan that read a comment as code
// would report a binding that is not there — or worse, hide the one that is behind a line comment on the same line.
func TestStripCommentsKeepsCallsAndDropsProse(t *testing.T) {
	cases := []struct {
		name            string
		src             string
		absent, present string // absent must NOT survive; present MUST
	}{
		{"a commented-out call is not a call", `// g.set("__cleat_a", x)`, `__cleat_a`, ``},
		{"a block-commented call is not a call", `/* g.set("__cleat_b", x) */`, `__cleat_b`, ``},
		{"a real call survives", `g.set("__cleat_c", x)`, ``, `__cleat_c`},
		{"a call after a line comment survives", "let u = 1; // note\ng.set(\"__cleat_d\", x)", `note`, `__cleat_d`},
		// The reason a plain `strings.Split(src, "//")` would be wrong: a `//` inside a string is not a comment, and
		// cutting there would delete the rest of the line, which is where the call is.
		{"a // inside a string does not start a comment", `let u = "http://x"; g.set("__cleat_e", x)`, ``, `__cleat_e`},
		// A block comment spanning lines must not join them into one line, or a scan anchored on `^` would move.
		{"a newline inside a block comment is kept", "a /* x\ny */ b", `x`, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripComments(c.src)
			if c.absent != "" && strings.Contains(got, c.absent) {
				t.Fatalf("stripComments left the comment text %q in %q", c.absent, got)
			}
			if c.present != "" && !strings.Contains(got, c.present) {
				t.Fatalf("stripComments dropped %q; it kept %q", c.present, got)
			}
		})
	}
	// The line structure is what a `^`-anchored scan depends on, so it is asserted separately from the text.
	if n := strings.Count(stripComments("a /* x\ny */ b"), "\n"); n != 1 {
		t.Fatalf("a newline inside a block comment left %d newlines, want 1: a line-anchored scan would move", n)
	}
}

// TestBindingScanReadsEveryGroupFile is the known-positive for the split: a call installed from a group's Rust file, and
// one declared in a group's TypeScript file, must be FOUND, and a mention in a comment there must not be. Without it a scan
// that quietly went back to reading lib.rs and index.ts alone would still pass on a tree with no group bound, and would
// report every call a group binds as a phantom the day the first one does.
func TestBindingScanReadsEveryGroupFile(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("crates/cleat-js-plugin/src/lib.rs", `g.set("__cleat_in_lib", f)`)
	write("crates/cleat-js-plugin/src/groups/locks.rs", "// g.set(\"__cleat_only_in_a_comment\", f)\ng.set(\"__cleat_in_a_group\", f)\n")
	write("packages/cleat-ts/index.ts", `declare function __cleat_ts_in_index(a: string): string;`)
	write("packages/cleat-ts/groups/locks.ts", "// declare function __cleat_ts_only_in_a_comment(a: string): string;\ndeclare function __cleat_ts_in_a_group(a: string): string;\n")

	got := pluginGlobals(t, root)
	if strings.Join(got, ",") != "__cleat_in_a_group,__cleat_in_lib" {
		t.Fatalf("pluginGlobals read %v, want exactly the two installed globals (one from lib.rs, one from src/groups/)", got)
	}
	got = declaredGlobals(t, root)
	if strings.Join(got, ",") != "__cleat_ts_in_a_group,__cleat_ts_in_index" {
		t.Fatalf("declaredGlobals read %v, want exactly the two declarations (one from index.ts, one from groups/)", got)
	}
}

// ---- two groups must not add the same Ctx method -----------------------------------------------------------------------
//
// Nothing runs tsc on packages/cleat-ts, and `ctx` is built by spreading each group's object into one literal, so two groups
// that each add `ctx.foo` would not fail to build: the later spread would silently win, and one group's method would answer
// with the other's behaviour. This is the check that stands in for the compiler's "duplicate identifier".

var (
	ctxInterfaceRE = regexp.MustCompile(`(?m)^export interface (Ctx|[A-Z][A-Za-z]*Ctx)\b[^{\n]*\{[ \t]*$`)
	ctxMemberRE    = regexp.MustCompile(`^  (?:readonly\s+)?([A-Za-z_$][A-Za-z0-9_$]*)\s*[(<:?]`)
)

// ctxMembers returns the member names declared at the top level (two-space indent) of every `export interface Ctx` and
// `export interface <Group>Ctx` in src, comments stripped first so a method named in prose is not a method.
func ctxMembers(src string) []string {
	lines := strings.Split(stripComments(src), "\n")
	var out []string
	in := false
	for _, l := range lines {
		switch {
		case !in && ctxInterfaceRE.MatchString(l):
			in = true
		case in && l == "}":
			in = false
		case in:
			if m := ctxMemberRE.FindStringSubmatch(l); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// ctxCollisions reports every member name that more than one file declares. The map is file name -> that file's source.
func ctxCollisions(sources map[string]string) []string {
	owners := map[string][]string{}
	for _, file := range sortedKeys(sources) {
		seen := map[string]bool{}
		for _, name := range ctxMembers(sources[file]) {
			if !seen[name] {
				seen[name] = true
				owners[name] = append(owners[name], file)
			}
		}
	}
	var v []string
	for _, name := range sortedKeys(owners) {
		if len(owners[name]) > 1 {
			v = append(v, fmt.Sprintf("the Ctx method %q is declared by %s: the later spread in index.ts would silently replace the earlier, so rename one", name, strings.Join(owners[name], " and ")))
		}
	}
	return v
}

func TestNoTwoGroupsAddTheSameCtxMethod(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "packages", "cleat-ts")
	sources := map[string]string{}
	files := append([]string{filepath.Join(dir, "index.ts")}, walkFiles(t, filepath.Join(dir, "groups"), ".ts")...)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, f)
		sources[filepath.ToSlash(rel)] = string(b)
	}
	// Not vacuous: index.ts declares the whole pre-split surface, so a scan that read none of it is broken, not clean.
	if n := len(ctxMembers(sources["index.ts"])); n < 20 {
		t.Fatalf("read %d members of Ctx in index.ts, want the 30-odd it declares: the interface scan is broken", n)
	}
	if v := ctxCollisions(sources); len(v) > 0 {
		t.Fatalf("%s", strings.Join(v, "\n"))
	}
}

// TestCtxCollisionScanSeesPlantedShapes holds the scan to cases already known to collide, and to ones that must not.
func TestCtxCollisionScanSeesPlantedShapes(t *testing.T) {
	iface := func(name string, members ...string) string {
		return "export interface " + name + " {\n" + strings.Join(members, "\n") + "\n}\n"
	}
	cases := []struct {
		name    string
		sources map[string]string
		want    string // a substring of a reported collision; empty means none may be reported
	}{
		{"two groups, one method name", map[string]string{
			"groups/locks.ts":   iface("LocksCtx", "  tryLock(key: string): Promise<boolean>;"),
			"groups/signals.ts": iface("SignalsCtx", "  tryLock(key: string): Promise<boolean>;"),
		}, `"tryLock"`},
		{"a group method that shadows a core one", map[string]string{
			"index.ts":        iface("Ctx extends LocksCtx", "  sleep(ms: number): Promise<void>;"),
			"groups/locks.ts": iface("LocksCtx", "  sleep<T>(ms: number): Promise<T>;"),
		}, `"sleep"`},
		{"a property and a method of one name", map[string]string{
			"groups/a.ts": iface("ACtx", "  now: () => number;"),
			"groups/b.ts": iface("BCtx", "  now(): number;"),
		}, `"now"`},
		{"a readonly property shadows a core method", map[string]string{
			"index.ts":        iface("Ctx extends LocksCtx", "  sleep(ms: number): Promise<void>;"),
			"groups/locks.ts": iface("LocksCtx", "  readonly sleep: (ms: number) => Promise<void>;"),
		}, `"sleep"`},
		{"distinct names do not collide", map[string]string{
			"groups/a.ts": iface("ACtx", "  one(): void;"),
			"groups/b.ts": iface("BCtx", "  two(): void;"),
		}, ""},
		{"a name in a comment is not a member", map[string]string{
			"groups/a.ts": iface("ACtx", "  // tryLock(key) is the second spelling", "  one(): void;"),
			"groups/b.ts": iface("BCtx", "  tryLock(key: string): void;"),
		}, ""},
		{"a nested object type's keys are not members", map[string]string{
			"groups/a.ts": iface("ACtx", "  one(): {", "    two: number;", "  };"),
			"groups/b.ts": iface("BCtx", "  two(): void;"),
		}, ""},
		{"a non-Ctx interface is ignored", map[string]string{
			"groups/a.ts": iface("ACtx", "  one(): void;") + iface("Scope", "  one: string;"),
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := ctxCollisions(c.sources)
			if c.want == "" {
				if len(v) > 0 {
					t.Fatalf("reported a collision where there is none: %v", v)
				}
				return
			}
			for _, msg := range v {
				if strings.Contains(msg, c.want) {
					return
				}
			}
			t.Fatalf("did not report the planted collision %s; got %v", c.want, v)
		})
	}
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
