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
//	crates/cleat-js-plugin/src/lib.rs        what the plugin actually installs (g.set("__cleat_…"))
//	packages/cleat-ts/index.ts               what TypeScript is told exists (declare function __cleat_…)
//	packages/cleat-ts/host-calls.json        the decided mapping, plus every unbound call with its reason
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
}

// bindingSources is everything reconcileBindings compares. It is a value so the self-test below can feed it synthetic
// inputs: a reconciler with no known-positive is a claim that it works, and every surviving bug in this file's family
// was a check that passed a tree it should have failed.
type bindingSources struct {
	exported  []string // engine/imports.go
	installed []string // crates/cleat-js-plugin/src/lib.rs
	declared  []string // packages/cleat-ts/index.ts
	inventory bindingInventory
}

// handshakeExports are the two worker handshake calls, which no guest can reach, and the one export ABI deliberately
// makes unbindable. They must stay listed as "permanent": if one is ever bound, that is a change to the ABI contract and
// worth failing on rather than quietly allowing.
var handshakeExports = []string{"cleat_poll_work", "cleat_complete", "cleat_register_query_handler"}

var bindingReasonPrefix = regexp.MustCompile(`^(pending|permanent): \S.{15,}$`)

// reconcileBindings returns one message per violation, in a stable order. Empty means the inventory and the three
// sources agree.
func reconcileBindings(s bindingSources) []string {
	var v []string
	exported := toSet(s.exported)
	installed := toSet(s.installed)
	declared := toSet(s.declared)

	boundGlobals := sortedKeys(s.inventory.Bound)
	plumbing := toSet(s.inventory.Plumbing)

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
		v = append(v, fmt.Sprintf("crates/cleat-js-plugin/src/lib.rs installs %s, but host-calls.json lists it as neither plumbing nor bound", g))
	}
	for _, g := range append(sortedKeys(plumbing), boundGlobals...) {
		if _, ok := installed[g]; !ok {
			v = append(v, fmt.Sprintf("host-calls.json lists %s, but the plugin never installs it (no g.set(\"%s\", …) in lib.rs)", g, g))
		}
	}

	// TypeScript declares everything it can call. A bound global that index.ts does not declare is a host call with no
	// TypeScript signature, which is half of what the issue asks for. Plumbing may go undeclared: index.ts reads
	// __cleat_args/result/fail/suspended, while __cleat_shims_off is read by shims.js, not by the SDK.
	for _, g := range boundGlobals {
		if _, ok := declared[g]; !ok {
			v = append(v, fmt.Sprintf("%s is bound but packages/cleat-ts/index.ts does not declare it, so no TypeScript signature exists", g))
		}
	}
	for _, g := range sortedKeys(declared) {
		if _, ok := installed[g]; !ok {
			v = append(v, fmt.Sprintf("packages/cleat-ts/index.ts declares %s, which the plugin never installs: a call to it fails with \"not a function\"", g))
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
			v = append(v, fmt.Sprintf("host-calls.json lists %s as unbound with no reason", name))
		case listed && !bindingReasonPrefix.MatchString(reason):
			v = append(v, fmt.Sprintf("host-calls.json's reason for %s does not start \"pending: \" or \"permanent: \" with an explanation: %q", name, reason))
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
		exported:  engineExports(t, root),
		installed: pluginGlobals(t, root),
		declared:  declaredGlobals(t, root),
		inventory: loadInventory(t, root),
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
		{"a global installed but unmapped", "lists it as neither plumbing nor bound", func(s *bindingSources) {
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
		{"a handshake call no longer marked permanent", "must stay listed", func(s *bindingSources) {
			s.inventory.Unbound["cleat_poll_work"] = "pending: maybe a workflow could poll its own queue?"
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

func loadInventory(t *testing.T, root string) bindingInventory {
	t.Helper()
	path := filepath.Join(root, "packages", "cleat-ts", "host-calls.json")
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
	return inv
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

func pluginGlobals(t *testing.T, root string) []string {
	t.Helper()
	return scanFile(t, filepath.Join(root, "crates", "cleat-js-plugin", "src", "lib.rs"), pluginSetRE)
}

func declaredGlobals(t *testing.T, root string) []string {
	t.Helper()
	return scanFile(t, filepath.Join(root, "packages", "cleat-ts", "index.ts"), tsDeclarationRE)
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
