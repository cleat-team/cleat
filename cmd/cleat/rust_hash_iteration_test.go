package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rule is about ORDER, not about the type. Using a HashMap is fine and
// common; exposing its iteration order is the thing worth a word. A checker
// that cannot hold that line becomes a rule people argue with instead of one
// they fix, so the negative cases below carry as much weight as the positive.
func TestRustHashIterationIsReported(t *testing.T) {
	const preamble = "use std::collections::HashMap;\nuse std::collections::HashSet;\n"

	for _, tc := range []struct {
		name string
		body string
		want bool
		why  string
	}{
		// --- reported ---
		{"for over a reference", "let m: HashMap<String, u64> = f();\nfor (k, v) in &m { g(k, v); }", true,
			"the loop form names no method at all, so a method-only matcher misses it"},
		{"for over the value", "let m: HashMap<String, u64> = f();\nfor kv in m { g(kv); }", true,
			"consuming iteration is still iteration"},
		{"iter()", "let m: HashMap<String, u64> = f();\nlet xs = m.iter();", true, "the plain method form"},
		{"keys()", "let m: HashMap<String, u64> = f();\nfor k in m.keys() { g(k); }", true,
			"keys are as ordered as entries are"},
		{"values()", "let m: HashMap<String, u64> = f();\nlet t = m.values();", true, ""},
		{"drain()", "let mut m: HashMap<String, u64> = f();\nlet d = m.drain();", true,
			"drain yields in iteration order too"},
		{"HashSet", "let s: HashSet<u64> = f();\nfor x in &s { g(x); }", true,
			"HashSet has the same randomised hasher; omitting it would be an arbitrary half-rule"},
		{"constructed, not annotated", "let mut m = HashMap::new();\nm.insert(1, 2);\nfor kv in &m { g(kv); }", true,
			"the binding's type comes from the constructor, which is the more common Rust idiom"},
		{"with_capacity", "let mut m = HashMap::with_capacity(8);\nlet xs = m.iter();", true, ""},
		{"a fn parameter", "fn total(items: &HashMap<String, u64>) -> u64 {\n    items.values().sum()\n}", true,
			"a map arriving as a parameter is the shape the issue was filed on"},

		// --- not reported ---
		{"get", "let m: HashMap<String, u64> = f();\nlet v = m.get(&k);", false,
			"lookup does not expose order"},
		{"insert", "let mut m: HashMap<String, u64> = f();\nm.insert(k, v);", false, "nor does insertion"},
		{"contains_key", "let m: HashMap<String, u64> = f();\nlet b = m.contains_key(&k);", false, ""},
		{"len", "let m: HashMap<String, u64> = f();\nlet n = m.len();", false, ""},
		{"remove and entry", "let mut m: HashMap<String, u64> = f();\nm.remove(&k);\nm.entry(k).or_insert(0);", false,
			"the two other common mutators"},
		{"BTreeMap iteration", "use std::collections::BTreeMap;\nlet m: BTreeMap<String, u64> = f();\nfor kv in &m { g(kv); }", false,
			"sorted order is GUARANTEED by the API -- this is the fix, and flagging it would send a reader in a circle"},
		{"BTreeSet iteration", "use std::collections::BTreeSet;\nlet s: BTreeSet<u64> = f();\nfor x in &s { g(x); }", false, ""},
		{"a Vec", "let v: Vec<u64> = f();\nfor x in &v { g(x); }", false,
			"insertion order, entirely deterministic"},
		{"iterating something else entirely", "let m: HashMap<String, u64> = f();\nlet v: Vec<u64> = f();\nfor x in &v { g(x); }", false,
			"a hash map being IN SCOPE must not make every nearby loop a finding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := findRustHashIteration([]byte(preamble + tc.body))
			if (len(got) > 0) != tc.want {
				t.Errorf("findRustHashIteration(...) reported %d findings, want reported=%v -- %s\nsource:\n%s",
					len(got), tc.want, tc.why, tc.body)
			}
		})
	}
}

// Aliases and the fully-qualified spelling, because the sibling path rules
// resolve both and a rule that only sees the bare name would be the substring
// matcher this file spent #1815 and #1817 getting away from.
func TestRustHashIterationResolvesAliasesAndQualifiedNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"an `as` alias", "use std::collections::HashMap as HM;\nlet m: HM<String, u64> = f();\nfor kv in &m { g(kv); }", true},
		{"a grouped use", "use std::collections::{HashMap, BTreeMap};\nlet m: HashMap<String, u64> = f();\nlet xs = m.iter();", true},
		{"fully qualified, no use", "let m: std::collections::HashMap<String, u64> = f();\nfor kv in &m { g(kv); }", true},
		{"a BTreeMap from the same grouped use", "use std::collections::{HashMap, BTreeMap};\nlet b: BTreeMap<String, u64> = f();\nfor kv in &b { g(kv); }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(findRustHashIteration([]byte(tc.src))) > 0; got != tc.want {
				t.Errorf("reported=%v, want %v\nsource:\n%s", got, tc.want, tc.src)
			}
		})
	}
}

// THE KNOWN LIMITS, as tests rather than as a paragraph nobody rereads.
//
// Each of these asserts the CURRENT behaviour, so the day one of them starts
// being handled the test fails and has to be moved rather than quietly
// disagreeing with the comment that claims it. Written after the egress scan in
// plugins/, which states its limits the same way for the same reason.
func TestRustHashIterationKnownLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		want   int
		lifted string
	}{
		{
			name:   "a map behind a struct field is not seen",
			src:    "use std::collections::HashMap;\nstruct S { counts: HashMap<String, u64> }\nfn go(s: &S) { for kv in &s.counts { g(kv); } }",
			want:   0,
			lifted: "struct fields are now matched",
		},
		{
			name:   "a map returned from a call is not seen",
			src:    "use std::collections::HashMap;\nfn go() { for kv in make_map().iter() { g(kv); } }",
			want:   0,
			lifted: "call results are now matched",
		},
		{
			// Deterministic regardless of order, and reported anyway. This is
			// the main source of noise and the main reason R101 is a warning.
			name:   "an order-independent consumer is still reported",
			src:    "use std::collections::HashMap;\nlet m: HashMap<String, u64> = f();\nlet total: u64 = m.values().sum();",
			want:   1,
			lifted: "order-independent consumers are now distinguished",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(findRustHashIteration([]byte(tc.src))); got != tc.want {
				t.Errorf("reported %d findings, want %d.\n\n"+
					"If this changed because %s, that is an IMPROVEMENT and this test is "+
					"the thing to update: move this case out of the known-limits list and "+
					"into the table above, and delete its line from the comment on "+
					"findRustHashIteration. The list and the code must not disagree.",
					got, tc.want, tc.lifted)
			}
		})
	}
}

// Comments and string literals must not produce findings, which is the defect
// #1782 fixed for the path rules. The blanking happens in the caller, so this
// pins that R101 is fed the SAME blanked source and not the raw bytes.
func TestRustHashIterationReadsCodeNotProse(t *testing.T) {
	src := "use std::collections::HashMap;\n" +
		"// Do not write `for kv in &m {}` over a HashMap here.\n" +
		"let doc = \"for kv in &m\";\n" +
		"let m: HashMap<String, u64> = f();\n" +
		"let v = m.get(&k);\n"
	if got := findRustHashIteration(rustCodeOnly([]byte(src))); len(got) != 0 {
		t.Errorf("reported %d findings on a comment and a string literal: %+v", len(got), got)
	}
}

// R101 must not change the exit code, which is the whole point of choosing a
// warning. Asserted on the reporting contract rather than by running the
// binary: Errors decide the exit, so a finding that lands in Warnings cannot.
func TestRustHashIterationIsAWarningAndDoesNotFailTheBuild(t *testing.T) {
	src := []byte("use std::collections::HashMap;\nlet m: HashMap<String, u64> = f();\nfor kv in &m { g(kv); }\n")

	findings := findRustHashIteration(src)
	if len(findings) == 0 {
		t.Fatal("the fixture stopped being reported; the rest of this test proves nothing")
	}
	if findings[0].code != rustHashIterCode {
		t.Errorf("code is %q, want %q", findings[0].code, rustHashIterCode)
	}
	if !strings.HasPrefix(rustHashIterCode, "R1") {
		t.Errorf("R101 is in the R0xx range (%s), which vet_rust.go uses for build-failing "+
			"errors; R1xx is the warning range and R100 is the existing member", rustHashIterCode)
	}
	// The message must say WHY it is only a warning, because a reader who is
	// told "unspecified in Rust" and sees their build pass will conclude the
	// checker is broken rather than that the runtime covers it.
	if !strings.Contains(rustHashIterMessage, "cleat intercepts") {
		t.Errorf("message does not say the determinism comes from the runtime "+
			"interception, so a passing build looks like a bug: %q", rustHashIterMessage)
	}
	if !strings.Contains(rustHashIterSuggestion, "BTreeMap") {
		t.Error("the suggestion does not name BTreeMap, which is the fix")
	}
}

// Positions are 1-based and point at the iteration, not at the declaration.
func TestRustHashIterationReportsThePositionOfTheIteration(t *testing.T) {
	src := []byte("use std::collections::HashMap;\n" + // 1
		"fn go() {\n" + // 2
		"    let m: HashMap<String, u64> = f();\n" + // 3
		"    for kv in &m { g(kv); }\n" + // 4
		"}\n")
	got := findRustHashIteration(src)
	if len(got) != 1 {
		t.Fatalf("want exactly one finding, got %d: %+v", len(got), got)
	}
	if got[0].line != 4 {
		t.Errorf("line = %d, want 4 (the loop, not the let on line 3)", got[0].line)
	}
	if got[0].col < 1 {
		t.Errorf("col = %d, want 1-based", got[0].col)
	}
}

// The fixture, through the REAL binary, because everything above tests a Go
// function and the property that matters is what `cleat vet` does.
//
// BOTH ARMS IN ONE TEST, for the reason TestRustBuildRefusesNondeterminism
// gives for the same shape: either alone passes against a tree where the rule
// does not exist. "It warns" is satisfied by nothing if the text arm is absent;
// "it does not refuse" is satisfied perfectly by a rule that was never wired.
func TestRustVetWarnsOnHashIterationWithoutRefusingTheCrate(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	dir := filepath.Join("..", "..", "testdata", "vet-checks", "rust", "r101_hash_iteration")
	out, err := exec.Command(cleatBinary, "vet", "--lang", "rust", dir).CombinedOutput()
	got := string(out)

	// ARM 1 -- it warns, and names the code and the fix.
	if !strings.Contains(got, "R101") {
		t.Errorf("no R101 in the output; the rule is not wired into runVetRust.\n%s", got)
	}
	if !strings.Contains(got, "BTreeMap") {
		t.Errorf("the output does not name BTreeMap, so a reader is told what is wrong "+
			"and not what to do about it.\n%s", got)
	}

	// ARM 2 -- and it does not refuse. This is the arm that pins the SEVERITY,
	// which is the whole design decision; see findRustHashIteration.
	if err != nil {
		t.Errorf("vet exited non-zero (%v) on a crate whose only findings are warnings. "+
			"R101 must not fail a build: the code it fires on is deterministic today, "+
			"and refusing it would reject idiomatic Rust that works.\n%s", err, got)
	}
	if strings.Contains(got, "Error [R101]") {
		t.Errorf("R101 was reported as an Error rather than a Warning.\n%s", got)
	}
	if !strings.Contains(got, "0 errors") {
		t.Errorf("the summary reports errors on a warnings-only fixture.\n%s", got)
	}

	// ARM 3 -- the negative control INSIDE the fixture. get, insert and len sit
	// in the same function as the reported loop; if the rule fired on the type
	// rather than on iteration, these would produce findings too and the count
	// would be higher than the two iteration sites.
	if n := strings.Count(got, "[R101]"); n != 2 {
		t.Errorf("R101 fired %d times, want exactly 2 (the for-loop and the keys() call). "+
			"The fixture also calls get, insert and len on the same binding, which expose "+
			"no order and must stay silent.\n%s", n, got)
	}
}
