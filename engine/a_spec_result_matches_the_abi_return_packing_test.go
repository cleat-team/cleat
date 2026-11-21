package engine

// Every step spec's declared Result matches the return packing ABI.md documents for its host call (cleat#4297).
//
// validate() proves a spec DECLARES a result layout. It cannot prove the layout is the RIGHT one, and a wrong one is invisible: cleat-review3 declared
// await_child and delete_cron as layoutDurableCall and the whole ./engine stayed green, while a guest on the other layout would have read a garbage
// call-error code and a one-byte message from a divergence. The six G3 moves are where that mistake would be made.
//
// THE SECOND DERIVATION IS ABI.md, NOT THE SPECS. A test that derived the layout from a spec's own Fresh or Decode would be one author's reading of the
// same code agreeing with itself (the repo already records that trap: two derivations that share a denominator agree and prove nothing). ABI.md is what
// the guests are written against, so it is the independent source. The layout is decided by WHERE THE LENGTH FIELD SITS in the documented return packing:
//
//	responseLen 40-63 with callErrorCode 8-39           durableCall
//	payloadLen 32-47 with sigNameLen 48-63              awaitSignals
//	a field at 32-63, or errCode alone in the low bits  simple   (flag fields in between, as await_promise's timedOut, do not change it: a
//	                                                              simple divergence decodes compatibly there)
//
// WHAT THIS CANNOT DO, so nobody reads it as more. It reads prose and tables with a small parser, and ABI.md is not uniform (a table, "errCode only",
// "name lo-hi" fields in a sentence, and "the same shape as `X`" references all occur). A block the parser cannot classify is UNMEASURED and FAILS BY
// NAME; it never passes. A spec with no ABI section is UNMEASURED by name too, which is also what forces every new spec to be classified. The parser
// has its own controls below: a reading of ABI.md that is wrong would vouch for every spec, so it is checked against layouts derived by hand for all three.
//
// go test caches on files opened inside the package directory, and ABI.md is read through "..", so a local run after editing ABI.md can say
// "(cached)". CI passes -count=1.

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type abiField struct {
	name   string
	lo, hi int
}

// abiImportFor maps a spec's Name to its host call's ABI import. The exceptions are explicit and visible: a name that is not here must be the import
// itself or cleat_<name>, and a spec that resolves to nothing is UNMEASURED.
func abiImportFor(name string, imports map[string]string) (string, bool) {
	exceptions := map[string]string{
		"durable_send": "cleat_send", // §2.46: the spec is named for the event it records, the import for the call it answers
	}
	if imp, ok := exceptions[name]; ok {
		_, found := imports[imp]
		return imp, found
	}
	for _, cand := range []string{name, "cleat_" + name} {
		if _, ok := imports[cand]; ok {
			return cand, true
		}
	}
	return "", false
}

// abiSections splits ABI.md into its "#### " sections and indexes them by the host import each declares.
func abiSections(abi string) map[string]string {
	out := map[string]string{}
	imp := regexp.MustCompile(`\(import "env" "([^"]+)"\)`)
	for _, sec := range regexp.MustCompile(`(?m)^#### `).Split(abi, -1) {
		if m := imp.FindStringSubmatch(sec); m != nil {
			out[m[1]] = "#### " + sec
		}
	}
	return out
}

var (
	abiTableRow  = regexp.MustCompile("(?m)^\\|\\s*(\\d+)-(\\d+)\\s*\\|\\s*`(\\w+)`")
	abiProseSpan = regexp.MustCompile("`?(\\w+)`?\\s+(\\d+)-(\\d+)")
	abiBytesSpan = regexp.MustCompile(`bits (\d+)-(\d+) the number of bytes`)
	abiOnly      = regexp.MustCompile("`errCode` only")
	abiSameAs    = regexp.MustCompile("same\\s+(?:shape|layout)\\s+as\\s+`(\\w+)`|same\\s+\\d+/\\d+\\s+split\\s+used\\s+by\\s+`(\\w+)`")
)

// returnPackingBlock is the paragraph that follows "Return packing" in a section, plus the table directly after it when the paragraph only introduces one.
func returnPackingBlock(sec string) (string, bool) {
	i := strings.Index(sec, "Return packing")
	if i < 0 {
		return "", false
	}
	paras := regexp.MustCompile(`\n\s*\n`).Split(sec[i:], -1)
	block := paras[0]
	if len(paras) > 1 && strings.HasPrefix(strings.TrimSpace(paras[1]), "|") {
		block += "\n\n" + paras[1]
	}
	return block, true
}

// classifyABI returns the layout a section's documented return packing implies, or an error saying why it could not tell (UNMEASURED, never a guess).
func classifyABI(imp string, secs map[string]string, depth int) (resultLayout, string, error) {
	if depth > 3 {
		return layoutUnset, "", &abiErr{"a chain of 'same shape as' references longer than 3 for " + imp}
	}
	sec, ok := secs[imp]
	if !ok {
		return layoutUnset, "", &abiErr{"no ABI.md section declares the import " + imp}
	}
	block, ok := returnPackingBlock(sec)
	if !ok {
		return layoutUnset, "", &abiErr{imp + " has no \"Return packing\" text"}
	}
	var fields []abiField
	for _, m := range abiTableRow.FindAllStringSubmatch(block, -1) {
		lo, _ := strconv.Atoi(m[1])
		hi, _ := strconv.Atoi(m[2])
		fields = append(fields, abiField{m[3], lo, hi})
	}
	for _, m := range abiProseSpan.FindAllStringSubmatch(block, -1) {
		if m[1] == "bits" || m[1] == "Bits" {
			continue
		}
		lo, _ := strconv.Atoi(m[2])
		hi, _ := strconv.Atoi(m[3])
		fields = append(fields, abiField{m[1], lo, hi})
	}
	if m := abiBytesSpan.FindStringSubmatch(block); m != nil {
		lo, _ := strconv.Atoi(m[1])
		hi, _ := strconv.Atoi(m[2])
		fields = append(fields, abiField{"bytesWritten", lo, hi})
	}
	if len(fields) == 0 {
		if abiOnly.MatchString(block) {
			fields = append(fields, abiField{"errCode", 0, 31})
		} else if m := abiSameAs.FindStringSubmatch(block); m != nil {
			ref := m[1] + m[2]
			l, why, err := classifyABI(ref, secs, depth+1)
			return l, imp + " is documented as the same shape as " + ref + " (" + why + ")", err
		}
	}
	has := func(lo, hi int) bool {
		for _, f := range fields {
			if f.lo == lo && f.hi == hi {
				return true
			}
		}
		return false
	}
	desc := func() string {
		var p []string
		for _, f := range fields {
			p = append(p, f.name+" "+strconv.Itoa(f.lo)+"-"+strconv.Itoa(f.hi))
		}
		return strings.Join(p, ", ")
	}
	allLow := len(fields) > 0
	for _, f := range fields {
		if f.hi > 31 {
			allLow = false
		}
	}
	switch {
	case has(40, 63) && has(8, 39):
		return layoutDurableCall, desc(), nil
	case has(32, 47) && has(48, 63):
		return layoutAwaitSignals, desc(), nil
	case has(32, 63):
		return layoutSimple, desc(), nil
	case allLow:
		return layoutSimple, desc() + " (no length field)", nil
	}
	return layoutUnset, "", &abiErr{"cannot classify the return packing of " + imp + " (fields read: " + desc() + ")"}
}

type abiErr struct{ s string }

func (e *abiErr) Error() string { return e.s }

// checkSpecsAgainstABI is the property: every spec resolves to an ABI import, the import's documented return packing classifies, and the class equals
// the spec's declared Result. Violations and UNMEASURED lines are both failures and are told apart by their prefix.
func checkSpecsAgainstABI(specs []*stepSpec, abi string) []string {
	secs := abiSections(abi)
	var out []string
	for _, sp := range specs {
		imp, ok := abiImportFor(sp.Name, secs)
		if !ok {
			out = append(out, "UNMEASURED: spec "+sp.Name+" resolves to no ABI.md host import (tried "+sp.Name+" and cleat_"+sp.Name+"); add it to abiImportFor if its import is named differently. This is a failure of the check, not a finding about the spec.")
			continue
		}
		got, why, err := classifyABI(imp, secs, 0)
		if err != nil {
			out = append(out, "UNMEASURED: spec "+sp.Name+" ("+imp+"): "+err.Error()+". This is a failure of the check, not a finding about the spec.")
			continue
		}
		if got != sp.Result {
			out = append(out, "spec "+sp.Name+" declares "+layoutName(sp.Result)+" but ABI.md's return packing for "+imp+" is "+layoutName(got)+" ("+why+")")
		}
	}
	sort.Strings(out)
	return out
}

func readABI(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../ABI.md")
	if err != nil {
		t.Fatalf("UNMEASURED: cannot read ABI.md: %v", err)
	}
	return string(b)
}

// THE PROPERTY, on the real registry and the real ABI.md.
func TestEveryRegisteredSpecDeclaresTheLayoutItsABIReturnPackingImplies(t *testing.T) {
	if len(stepSpecs) < 15 {
		t.Fatalf("PRECONDITION FAILED: only %d specs registered, so this checks almost nothing", len(stepSpecs))
	}
	if v := checkSpecsAgainstABI(stepSpecs, readABI(t)); len(v) != 0 {
		t.Errorf("%d spec(s) disagree with ABI.md (or could not be checked):\n  %s", len(v), strings.Join(v, "\n  "))
	}
}

// CONTROL for the parser itself: a misreading of ABI.md would vouch for every spec, so its answer is compared with layouts derived BY HAND, one or more
// calls per layout and one per documented FORM (table, prose fields, "errCode only", a reference, a status table).
func TestTheABIParserAgreesWithLayoutsDerivedByHand(t *testing.T) {
	secs := abiSections(readABI(t))
	hand := []struct {
		imp  string
		want resultLayout
		form string
	}{
		{"cleat_await_child", layoutSimple, "table: errCode 0-31, resultLen 32-63"},
		{"cleat_await_all_children", layoutSimple, "table: errCode 0-7, resultLen 32-63"},
		{"cleat_await_promise", layoutSimple, "table with a flag field between errCode and the length"},
		{"cleat_acquire_lock", layoutSimple, "table: errCode only"},
		{"cleat_delete_cron", layoutSimple, "prose: `errCode` only"},
		{"cleat_schedule_cron", layoutSimple, "prose reference: the same 32/32 split used by another call"},
		{"cleat_side_effect_recorded", layoutSimple, "status table plus 'bits 32-63 the number of bytes written'"},
		{"cleat_call", layoutDurableCall, "table: errCode 0-7, callErrorCode 8-39, responseLen 40-63"},
		{"cleat_call_retry", layoutDurableCall, "table"},
		{"cleat_call_heartbeat", layoutDurableCall, "table"},
		{"plugin_call", layoutDurableCall, "table, an import with no cleat_ prefix"},
		{"plugin_call_streaming", layoutDurableCall, "table"},
		{"cleat_wait_for_event", layoutDurableCall, "prose fields: responseLen 40-63, callErrorCode 8-39, errCode 0-7"},
		{"cleat_await_signals", layoutAwaitSignals, "table: payloadLen 32-47, sigNameLen 48-63"},
	}
	seen := map[resultLayout]bool{}
	for _, h := range hand {
		got, why, err := classifyABI(h.imp, secs, 0)
		if err != nil {
			t.Errorf("%s (%s): the parser could not read it: %v", h.imp, h.form, err)
			continue
		}
		if got != h.want {
			t.Errorf("%s (%s): the parser says %s, derived by hand it is %s (%s)", h.imp, h.form, layoutName(got), layoutName(h.want), why)
		}
		seen[h.want] = true
	}
	for _, l := range []resultLayout{layoutSimple, layoutDurableCall, layoutAwaitSignals} {
		if !seen[l] {
			t.Errorf("PRECONDITION FAILED: no hand-derived control for %s, so a misparse of it would vouch for every spec", layoutName(l))
		}
	}
	// NEGATIVE CONTROL: a section the parser cannot read is an error, not a layout.
	bad := map[string]string{"cleat_x": "#### 2.99 `cleat_x`\n\n(func (import \"env\" \"cleat_x\"))\n\n**Return packing:** whatever the host feels like.\n"}
	if l, _, err := classifyABI("cleat_x", bad, 0); err == nil {
		t.Errorf("an unreadable return packing was classified as %s instead of being UNMEASURED", layoutName(l))
	}
}

// KNOWN POSITIVES, each driven through the same property on the real ABI.md. Each must fail BY NAME, and the unmutated run above is the control that
// the same registry passes.
func TestTheABIPropertyFailsForAWronglyDeclaredOrUnclassifiableSpec(t *testing.T) {
	abi := readABI(t)
	byName := map[string]*stepSpec{}
	for _, sp := range stepSpecs {
		byName[sp.Name] = sp
	}
	with := func(name string, l resultLayout) *stepSpec {
		src, ok := byName[name]
		if !ok {
			t.Fatalf("PRECONDITION FAILED: no registered spec named %s", name)
		}
		c := *src
		c.Result = l
		return &c
	}
	cases := []struct {
		label string
		specs []*stepSpec
		want  string
	}{
		// cleat-review3's two mutants, green across the whole ./engine before this test existed.
		{"await_child declared durable", []*stepSpec{with("await_child", layoutDurableCall)}, "spec await_child declares durableCall but ABI.md's return packing for cleat_await_child is simple"},
		{"delete_cron declared durable", []*stepSpec{with("delete_cron", layoutDurableCall)}, "spec delete_cron declares durableCall but ABI.md's return packing for cleat_delete_cron is simple"},
		// The case G3 will hit: a durable-call-layout call declared simple.
		{"a heartbeat-shaped spec declared simple", []*stepSpec{{Name: "call_heartbeat", Result: layoutSimple}}, "spec call_heartbeat declares simple but ABI.md's return packing for cleat_call_heartbeat is durableCall"},
		{"await_signals declared simple", []*stepSpec{with("await_signals", layoutSimple)}, "spec await_signals declares simple but ABI.md's return packing for cleat_await_signals is awaitSignals"},
		// A spec with no ABI row is UNMEASURED by name, never a pass.
		{"a spec with no ABI section", []*stepSpec{{Name: "no_such_host_call", Result: layoutSimple}}, "UNMEASURED: spec no_such_host_call resolves to no ABI.md host import"},
	}
	for _, c := range cases {
		got := strings.Join(checkSpecsAgainstABI(c.specs, abi), " | ")
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: want a result containing %q, got %q", c.label, c.want, got)
		}
	}
	// durable_send is the one name that is not cleat_<name>; with its exception it resolves and passes.
	if v := checkSpecsAgainstABI([]*stepSpec{with("durable_send", layoutSimple)}, abi); len(v) != 0 {
		t.Errorf("durable_send (ABI §2.46 cleat_send) did not resolve or classify: %v", v)
	}
}

// The documentation defect this test surfaced: ABI §2.57 said list_crons returns "in the same shape as `cleat_call`" (the durable-call layout), while the
// engine packs it with packSimpleResult and the Rust and Java guests decode it as simple. The corrected sentence is in ABI.md; putting the OLD one back
// must make the property fail by name, which proves the property is what would have caught it.
func TestTheABIPropertyCatchesTheListCronsDocumentationDefect(t *testing.T) {
	abi := readABI(t)
	fixed := "**Return packing:** the simple layout: `errCode` 0-31 and `resultLen` 32-63 (bytes\nwritten).\n\nThis said"
	if strings.Count(abi, fixed) != 1 {
		t.Fatalf("PRECONDITION FAILED: ABI.md does not carry the corrected list_crons sentence exactly once, so this test cannot restore the old one")
	}
	broken := strings.Replace(abi, fixed, "**Return packing:** `errCode` / bytes written, in the same shape as\n`cleat_call`.\n\nThis said", 1)
	var listCrons *stepSpec
	for _, sp := range stepSpecs {
		if sp.Name == "list_crons" {
			listCrons = sp
		}
	}
	if listCrons == nil {
		t.Fatal("PRECONDITION FAILED: no registered spec named list_crons")
	}
	if v := checkSpecsAgainstABI([]*stepSpec{listCrons}, abi); len(v) != 0 {
		t.Fatalf("PRECONDITION FAILED: list_crons does not pass against the corrected ABI.md: %v", v)
	}
	got := strings.Join(checkSpecsAgainstABI([]*stepSpec{listCrons}, broken), " | ")
	if !strings.Contains(got, "spec list_crons declares simple but ABI.md's return packing for cleat_list_crons is durableCall") {
		t.Errorf("the old list_crons sentence was not caught by name: %q", got)
	}
}
