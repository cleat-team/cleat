package main

import (
	"strings"
	"testing"
)

// cleat#1811. The checker resolves `use` declarations and matches the RESOLVED
// path, replacing a table of literal spellings that idiomatic Rust defeats
// without trying.
//
// WHY A UNIT TEST BESIDE THE FIXTURES. The fixtures prove the pipeline agrees;
// these say which FORM is handled. A resolver that flattened every group into
// one name, or that ignored `as`, passes a fixture suite whose crates each use
// one form — and the forms are where this is hard.
func TestExpandRustUseFlattensGroupsAndAliases(t *testing.T) {
	cases := []struct {
		name string
		decl string
		want map[string]string
	}{{
		name: "a plain import",
		decl: "std::fs",
		want: map[string]string{"fs": "std::fs"},
	}, {
		name: "an item import binds the item, not the module",
		decl: "std::fs::read_to_string",
		want: map[string]string{"read_to_string": "std::fs::read_to_string"},
	}, {
		name: "a group",
		decl: "std::{fs, net}",
		want: map[string]string{"fs": "std::fs", "net": "std::net"},
	}, {
		name: "a NESTED group -- the prefix accumulates down each arm",
		decl: "std::{collections::HashMap, io::{self, Write}}",
		want: map[string]string{
			"HashMap": "std::collections::HashMap",
			"io":      "std::io",
			"Write":   "std::io::Write",
		},
	}, {
		name: "an alias",
		decl: "std::time::SystemTime as ST",
		want: map[string]string{"ST": "std::time::SystemTime"},
	}, {
		name: "an alias INSIDE a group",
		decl: "std::net::{TcpStream, UdpSocket as US}",
		want: map[string]string{"TcpStream": "std::net::TcpStream", "US": "std::net::UdpSocket"},
	}, {
		name: "self brings the module in under its own name",
		decl: "std::io::self",
		want: map[string]string{"io": "std::io"},
	}, {
		name: "a glob names nothing locally, so nothing resolves through it",
		decl: "std::fs::*",
		want: map[string]string{},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			expandRustUse(tc.decl, got)

			if len(got) != len(tc.want) {
				t.Fatalf("resolved %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%q resolved to %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestFindForbiddenRustPathsSeesWhatSpellingsMissed is the known-positive set:
// every form in it was invisible to the table this replaces.
func TestFindForbiddenRustPathsSeesWhatSpellingsMissed(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantCode string
		wantText string // a substring of the finding, or "" for no finding
	}{{
		name:     "a grouped import and a bare-name call",
		src:      "use std::{fs, io};\nfn f() { let _ = fs::read_to_string(\"x\"); }\n",
		wantCode: "R001",
		wantText: "std::fs::read_to_string",
	}, {
		name:     "an aliased clock",
		src:      "use std::time::SystemTime as ST;\nfn f() { let _ = ST::now(); }\n",
		wantCode: "R005",
		wantText: "std::time::SystemTime::now",
	}, {
		name:     "an aliased socket inside a group",
		src:      "use std::net::{UdpSocket as US};\nfn f() { let _ = US::bind(\"0\"); }\n",
		wantCode: "R002",
		wantText: "std::net::UdpSocket",
	}, {
		name:     "a fully qualified call with no import at all",
		src:      "fn f() { let _ = std::process::Command::new(\"true\"); }\n",
		wantCode: "R003",
		wantText: "std::process",
	}, {
		name: "std::time::Duration is NOT a finding",
		src:  "use std::time::Duration;\nfn f() { let _ = Duration::from_secs(1); }\n",
	}, {
		name: "a module not on the list is NOT a finding",
		src:  "use std::collections::HashMap;\nfn f() { let _ = HashMap::<i32,i32>::new(); }\n",
	}, {
		name: "a METHOD call names no module -- the documented limit",
		src:  "use std::time::SystemTime;\nfn f(t: SystemTime) { let _ = t.elapsed(); }\n",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findForbiddenRustPaths([]byte(tc.src))

			if tc.wantCode == "" {
				if len(got) != 0 {
					t.Errorf("reported %d finding(s) on code that reaches nothing forbidden: %+v",
						len(got), got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("reported nothing. Every case here is one the literal-spelling table "+
					"could not see, which is why it is a case.\n%s", tc.src)
			}
			var joined []string
			codeSeen := false
			for _, f := range got {
				joined = append(joined, f.code+" "+f.message)
				if f.code == tc.wantCode {
					codeSeen = true
				}
			}
			if !codeSeen {
				t.Errorf("no %s among %v", tc.wantCode, joined)
			}
			// ON THE TEXT: a finding of the wrong kind satisfies a count.
			if !strings.Contains(strings.Join(joined, " | "), tc.wantText) {
				t.Errorf("no finding mentions the resolved path %q. Reporting only what was "+
					"WRITTEN sends the reader to a name that is in no table.\n  got: %v",
					tc.wantText, joined)
			}
		})
	}
}

// TestFindForbiddenRustPathsReportsImportAndCallSeparately pins the two-site
// rule. They are different things to fix — a declared dependency and a line
// that runs — and collapsing them loses one.
func TestFindForbiddenRustPathsReportsImportAndCallSeparately(t *testing.T) {
	const src = "use std::fs;\n\nfn f() {\n    let _ = fs::read_to_string(\"x\");\n}\n"

	got := findForbiddenRustPaths([]byte(src))
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (the import on line 1 and the call on line 4): %+v",
			len(got), got)
	}
	if got[0].line != 1 || got[1].line != 4 {
		t.Errorf("lines %d and %d, want 1 and 4", got[0].line, got[1].line)
	}
}

// TestFindForbiddenRustPathsDoesNotReportTheSameReachTwiceOnALine is the
// de-duplication, and it is a regression the old table had: `use std::fs` and
// `std::fs::` both matched `use std::fs::File;`, so one line printed twice.
func TestFindForbiddenRustPathsDoesNotReportTheSameReachTwiceOnALine(t *testing.T) {
	const src = "use std::fs::File;\n"

	got := findForbiddenRustPaths([]byte(src))
	if len(got) != 1 {
		t.Errorf("got %d findings for one import, want 1: %+v", len(got), got)
	}
}
