package main

import (
	"strings"
	"testing"
)

// cleat#1812. The checker resolves imports (explicit, static, and wildcard
// against a known list) and fully-qualified names, and matches the RESOLVED
// path, replacing a table of literal spellings that ordinary Java -- not
// evasion -- defeats without trying.
//
// WHY A UNIT TEST BESIDE THE FIXTURES. The fixtures under
// testdata/vet-checks/java/ prove the pipeline agrees; these say which FORM
// is handled. A resolver that only tried the common case passes a fixture
// suite whose files each use one form -- and the forms are where this is
// hard.
func TestFindForbiddenJavaPathsSeesWhatSpellingsMissed(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantCode string
		wantText string // a substring of the finding, or "" for no finding
	}{{
		name:     "a static import and a bare call",
		src:      "import static java.lang.System.currentTimeMillis;\nclass C { long f() { return currentTimeMillis(); } }\n",
		wantCode: "J001",
		wantText: "java.lang.System.currentTimeMillis",
	}, {
		name:     "a fully qualified call with no import at all",
		src:      "class C { long f() { return java.time.Clock.systemUTC().millis(); } }\n",
		wantCode: "J007",
		wantText: "java.time.Clock",
	}, {
		name:     "an ordinary variable name does not save a JDBC handle",
		src:      "import java.sql.Connection;\nclass C { void f(Connection db) {} }\n",
		wantCode: "J015",
		wantText: "java.sql.Connection",
	}, {
		name:     "a wildcard import resolves a KNOWN forbidden class",
		src:      "import java.net.*;\nclass C { void f() { Socket s = null; } }\n",
		wantCode: "J014",
		wantText: "java.net.Socket",
	}, {
		name:     "reflection with a literal name",
		src:      "class C { Object f() throws Exception { return Class.forName(\"x\").newInstance(); } }\n",
		wantCode: "J020",
		wantText: "java.lang.Class.forName",
	}, {
		name: "ByteArrayInputStream is NOT a finding",
		src:  "class C { int f(byte[] b) { return new java.io.ByteArrayInputStream(b).read(); } }\n",
	}, {
		name: "IOException at a catch site is NOT a finding -- it is a type reference, not an operation",
		src:  "import java.io.IOException;\nclass C { void f() { try {} catch (IOException e) {} } }\n",
	}, {
		name: "SQLException in a throws clause is NOT a finding, for the same reason",
		src:  "import java.sql.SQLException;\nclass C { void f() throws SQLException {} }\n",
	}, {
		name: "Thread.currentThread() is NOT a finding -- only .sleep and construction are",
		src:  "class C { Thread f() { return Thread.currentThread(); } }\n",
	}, {
		name: "a plain Thread declaration is NOT a finding",
		src:  "class C { Thread t; }\n",
	}, {
		name:     "constructing a Thread IS a finding, class-level ban or not",
		src:      "class C { Thread f() { return new Thread(); } }\n",
		wantCode: "J011",
		wantText: "thread scheduling",
	}, {
		name: "java.time.Duration is NOT a finding -- a pure value type, only Clock and Instant are named",
		src:  "class C { java.time.Duration f() { return java.time.Duration.ofSeconds(1); } }\n",
	}, {
		name: "a method call on a local variable names no known class, and is not a finding",
		src:  "class C { void f(String s) { s.trim(); } }\n",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findForbiddenJavaPaths(javaCodeOnly([]byte(tc.src)))

			if tc.wantCode == "" {
				if len(got) != 0 {
					t.Errorf("reported %d finding(s) on code that reaches nothing forbidden: %+v",
						len(got), got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("reported nothing. Every case here is one the literal-spelling table "+
					"could not see (or wrongly did see), which is why it is a case.\n%s", tc.src)
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
			if !strings.Contains(strings.Join(joined, " | "), tc.wantText) {
				t.Errorf("no finding mentions %q. Reporting only what was WRITTEN sends the "+
					"reader to a name that is in no table.\n  got: %v", tc.wantText, joined)
			}
		})
	}
}

// TestFindForbiddenJavaPathsReportsImportAndCallSeparately pins the two-site
// rule, the same one the Rust resolver carries: a declared dependency and a
// line that runs are different things to fix, and collapsing them loses one.
func TestFindForbiddenJavaPathsReportsImportAndCallSeparately(t *testing.T) {
	const src = "import java.sql.Connection;\n\nclass C {\n    void f(Connection db) {}\n}\n"

	got := findForbiddenJavaPaths(javaCodeOnly([]byte(src)))
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (the import on line 1 and the parameter on line 4): %+v",
			len(got), got)
	}
	if got[0].line != 1 || got[1].line != 4 {
		t.Errorf("lines %d and %d, want 1 and 4", got[0].line, got[1].line)
	}
}

// TestFindForbiddenJavaPathsDoesNotReportTheSameReachTwiceOnALine is the
// de-duplication: the old table's "Connection con" and its package-level
// import row could both match one line before this replaced them, and the
// resolver has the same shape of hazard if a name is aliased two ways.
func TestFindForbiddenJavaPathsDoesNotReportTheSameReachTwiceOnALine(t *testing.T) {
	const src = "import java.util.Random;\nclass C { Random f() { return new Random(); } }\n"

	got := findForbiddenJavaPaths(javaCodeOnly([]byte(src)))
	line2 := 0
	for _, f := range got {
		if f.line == 2 {
			line2++
		}
	}
	if line2 != 1 {
		t.Errorf("line 2 (\"Random f() { return new Random(); }\") produced %d findings, want 1: %+v",
			line2, got)
	}
}
