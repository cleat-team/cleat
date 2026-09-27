package main

import (
	"strings"
	"testing"
)

// cleat#1864. findForbiddenRustPaths matches PATHS; this rule matches a USAGE
// PATTERN on a syntactically tracked binding, since HashMap/HashSet are not
// forbidden to reach at all. These are unit tests for the resolver pieces,
// beside the fixtures that prove the pipeline agrees -- same split as
// rust_vet_resolves_paths_test.go for #1811.
func TestResolveRustTypeHead(t *testing.T) {
	aliases := map[string]string{
		"HashMap": "std::collections::HashMap",
		"Map":     "std::collections::HashMap",
	}
	cases := []struct {
		name, text, want string
	}{
		{"a bare aliased identifier", "HashMap", "std::collections::HashMap"},
		{"a reference is stripped", "&HashMap", "std::collections::HashMap"},
		{"a mutable reference is stripped", "&mut HashMap", "std::collections::HashMap"},
		{"generics are stripped", "HashMap<String, u64>", "std::collections::HashMap"},
		{"reference, generics and an import alias together", "&HashMap<String, u64>", "std::collections::HashMap"},
		{"a renamed alias resolves through the map, not the spelling", "Map<u32, u32>", "std::collections::HashMap"},
		{"an inline full path needs no alias", "std::collections::HashMap<String, u64>", "std::collections::HashMap"},
		{"an unrelated type is returned as written", "Vec<u32>", "Vec"},
		{"an unaliased bare identifier is returned as written", "HashSet", "HashSet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRustTypeHead(tc.text, aliases); got != tc.want {
				t.Errorf("resolveRustTypeHead(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestRustParamBindingType(t *testing.T) {
	aliases := map[string]string{"HashMap": "std::collections::HashMap"}
	cases := []struct {
		name, param, wantName, wantResolved string
		wantOK                              bool
	}{
		{"a reference parameter", "items: &HashMap<String, u64>", "items", "std::collections::HashMap", true},
		{"a mut binding", "mut items: HashMap<String, u64>", "items", "std::collections::HashMap", true},
		{"self has no colon", "self", "", "", false},
		{"&self has no colon", "&self", "", "", false},
		{"&mut self has no colon", "&mut self", "", "", false},
		{"an ordinary non-map parameter", "n: u64", "n", "u64", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, resolved, ok := rustParamBindingType(tc.param, aliases)
			if ok != tc.wantOK || name != tc.wantName || resolved != tc.wantResolved {
				t.Errorf("rustParamBindingType(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.param, name, resolved, ok, tc.wantName, tc.wantResolved, tc.wantOK)
			}
		})
	}
}

func TestSplitRustTopLevelCommas(t *testing.T) {
	cases := []struct {
		name, s string
		want    []string
	}{
		{"plain", "a, b, c", []string{"a", " b", " c"}},
		{"a nested generic is not split", "m: HashMap<String, u64>, n: u32", []string{"m: HashMap<String, u64>", " n: u32"}},
		{"a nested paren type is not split", "f: Box<dyn Fn(u32, u32) -> u32>", []string{"f: Box<dyn Fn(u32, u32) -> u32>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitRustTopLevelCommas(tc.s)
			if len(got) != len(tc.want) {
				t.Fatalf("splitRustTopLevelCommas(%q) = %q, want %q", tc.s, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("part %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestFindRustMapIterationFindings exercises the whole pipeline: blank the
// source, find function spans, track bindings, find iteration-shaped uses.
func TestFindRustMapIterationFindings(t *testing.T) {
	src := `
use std::collections::HashMap;

pub fn total(items: &HashMap<String, u64>) -> Vec<String> {
    let mut out = Vec::new();
    for (k, v) in items {
        out.push(format!("{}={}", k, v));
    }
    out
}

pub fn safe_lookup(items: &HashMap<String, u64>, key: &str) -> Option<u64> {
    items.get(key).copied()
}

pub fn safe_construct() -> HashMap<String, u64> {
    let mut m = HashMap::new();
    m.insert("a".to_string(), 1);
    m
}

pub fn also_bad() {
    let scores: HashMap<String, u64> = HashMap::new();
    for key in scores.keys() {
        println!("{}", key);
    }
}
`
	code := withoutCfgTest(rustCodeOnly([]byte(src)))
	findings := findRustMapIterationFindings(code)

	byLine := map[int]rustFinding{}
	for _, f := range findings {
		byLine[f.line] = f
	}

	// The for-loop directly over the parameter (line 6, 1-indexed within src).
	lines := strings.Split(src, "\n")
	forLoopLine, keysLine := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "for (k, v) in items") {
			forLoopLine = i + 1
		}
		if strings.Contains(l, "for key in scores.keys()") {
			keysLine = i + 1
		}
	}
	if forLoopLine < 0 || keysLine < 0 {
		t.Fatalf("test fixture drifted: could not locate its own marker lines")
	}

	if f, ok := byLine[forLoopLine]; !ok || f.code != "R008" {
		t.Errorf("no R008 finding at the for-loop over a HashMap parameter (line %d); got %+v", forLoopLine, findings)
	}
	if f, ok := byLine[keysLine]; !ok || f.code != "R008" {
		t.Errorf("no R008 finding at scores.keys() (line %d); got %+v", keysLine, findings)
	}
	if len(findings) != 2 {
		t.Errorf("got %d findings, want exactly 2 (get/insert/HashMap::new are all deterministic "+
			"and must not be reported): %+v", len(findings), findings)
	}
}

// TestTheMapOrderMessageComposesIntoASentence pins the defect that shipped with
// R008: the message is built by substituting a `kind` into one template, and
// the two kinds are grammatically different things.
//
//	"keys()"                 -> a CALL, reads as a subject on its own
//	"a for-loop over a map"  -> a NOUN PHRASE
//
// The original template continued "... iteration order is not guaranteed by the
// language", which composes correctly with the first and fuses into nonsense
// with the second:
//
//	a for-loop over a map iteration order is not guaranteed by the language
//
// -- on a diagnostic that FAILS A BUILD, so it is the sentence a developer reads
// at the moment they are least inclined to be charitable. The loop form is also
// the more common of the two.
//
// The fix is a template that continues with a VERB, so any noun phrase composes.
// Asserted for both kinds, because fixing one arm by rewording it alone would
// leave the next `kind` added here to rediscover this.
func TestTheMapOrderMessageComposesIntoASentence(t *testing.T) {
	const preamble = "use std::collections::HashMap;\n"

	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"the for-loop arm", "fn f() {\n let m: HashMap<String, u64> = g();\n for kv in &m { h(kv); }\n}",
			"a for-loop over a map exposes an order"},
		{"the method arm", "fn f() {\n let m: HashMap<String, u64> = g();\n let _ = m.keys();\n}",
			"keys() exposes an order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := findRustMapIterationFindings([]byte(preamble + tc.src))
			if len(got) == 0 {
				t.Fatal("no finding; this test proves nothing about the message")
			}
			if !strings.HasPrefix(got[0].message, tc.want) {
				t.Errorf("message = %q,\nwant it to start %q -- the template must continue "+
					"with a VERB so that a noun-phrase kind composes into a sentence",
					got[0].message, tc.want)
			}
			// The specific regression: a template beginning with a noun fuses
			// with the noun-phrase kind and the reader gets a garbled clause.
			if strings.Contains(got[0].message, "map iteration order is") {
				t.Errorf("message fused into a noun phrase: %q", got[0].message)
			}
		})
	}
}
