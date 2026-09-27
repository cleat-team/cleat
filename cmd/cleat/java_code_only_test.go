package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJavaCodeOnly pins what the Java scanner treats as code.
//
// Three shapes of prose used to be scanned as if they could execute, each a
// build refusal since #1791 wired this checker into `cleat build --target java`
// (cleat#1820):
//
//	int x = 1; // System.currentTimeMillis()      a comment AFTER code
//	String s = "System.currentTimeMillis()";      a string literal
//	/*
//	  System.currentTimeMillis() described here   a block interior whose line
//	*/                                            does not begin with *
//
// # The tests that matter most are the ordering ones
//
// A blanker that handles each construct in isolation can still re-phase the
// rest of the file: a `//` inside a string swallows the line, and a stray `"`
// inside a comment opens a string that runs to the next quote, hiding live code
// in between. Those two directions are asserted below, and both fail if the
// scanner checks constructs in the wrong order rather than lexing in sequence.
//
// # Why blanking and not deleting
//
// The caller reports 1-based line and column straight out of this result, so
// shortening anything would move every position it reports. Asserted here by
// length and by newline count, and end to end in the sibling test: a violation
// on line 7 after a four-line block comment is still reported on line 7.
func TestJavaCodeOnly(t *testing.T) {
	const marker = "System.currentTimeMillis()"

	cases := []struct {
		name    string
		src     string
		visible bool // is the marker still present after blanking?
	}{
		{
			name:    "a line comment is blanked",
			src:     "int x = 1;\n// " + marker + "\n",
			visible: false,
		},
		{
			name:    "a comment AFTER code is blanked",
			src:     "int x = 1; // " + marker + "\n",
			visible: false,
		},
		{
			name:    "a block comment interior is blanked, whatever the line starts with",
			src:     "/*\n  " + marker + " described here\n*/\nint x = 1;\n",
			visible: false,
		},
		{
			name:    "a string literal is blanked",
			src:     "String s = \"" + marker + "\";\n",
			visible: false,
		},
		{
			name:    "a text block is blanked",
			src:     "String s = \"\"\"\n  " + marker + "\n  \"\"\";\n",
			visible: false,
		},
		{
			name:    "real code is left alone",
			src:     "return " + marker + ";\n",
			visible: true,
		},
		{
			name:    "code followed by an unrelated comment is left alone",
			src:     "return " + marker + "; // fine\n",
			visible: true,
		},

		// ORDERING. Each of these has a construct that, handled out of
		// sequence, would consume the rest of the file and hide live code.
		{
			name:    "a // inside a string does not swallow the line",
			src:     "String url = \"http://x/y\";\nreturn " + marker + ";\n",
			visible: true,
		},
		{
			name:    "a stray quote inside a comment does not open a string",
			src:     "// a stray \" quote in prose\nreturn " + marker + ";\n",
			visible: true,
		},
		{
			name:    "an escaped quote does not end the string early",
			src:     "String s = \"a \\\"quoted\\\" thing\";\nreturn " + marker + ";\n",
			visible: true,
		},
		{
			name:    "a character literal does not open a string",
			src:     "char c = '\\'';\nreturn " + marker + ";\n",
			visible: true,
		},
		{
			name: "a block comment does not nest in Java, so the first */ closes it",
			// If this were treated as nesting (as Rust's does), the depth
			// counter would still be open at `return` and the call would be
			// hidden. Java closes at the first */, so the call is live code.
			src:     "/* outer /* inner */\nreturn " + marker + ";\n",
			visible: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(javaCodeOnly([]byte(tc.src)))

			if seen := strings.Contains(got, marker); seen != tc.visible {
				t.Errorf("marker visible=%v, want %v\n\nin:\n%s\nout:\n%s",
					seen, tc.visible, tc.src, got)
			}

			// Offsets must survive, or every reported line and column moves.
			if len(got) != len(tc.src) {
				t.Errorf("length changed: %d -> %d. Blanking must not shorten the "+
					"source; the caller reports positions straight out of this.",
					len(tc.src), len(got))
			}
			if a, b := strings.Count(tc.src, "\n"), strings.Count(got, "\n"); a != b {
				t.Errorf("newline count changed: %d -> %d, so every line number "+
					"after the change would be wrong", a, b)
			}
		})
	}
}

// TestTheJavaScanActuallyUsesJavaCodeOnly asserts that the scanner is WIRED IN,
// which TestJavaCodeOnly above cannot do.
//
// That test calls javaCodeOnly directly, so it passes whether or not
// runVetJava calls it. Verified by removing the call: the unit tests stayed
// green and the defect came back. A test that exercises a function in isolation
// says nothing about whether the program reaches it, and this package has a
// standing habit of finding that out the expensive way.
//
// So this one goes through runVetJava and asserts on the outcome a user gets.
func TestTheJavaScanActuallyUsesJavaCodeOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a project to disk and runs the vet over it")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build.gradle"),
		[]byte("plugins { id 'java' }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A comment AFTER code. The prefix test this replaced skipped only lines
	// whose FIRST non-space was a comment marker, so this one was scanned as
	// code and refused the build.
	src := "public class Main {\n" +
		"    public static int workflow() {\n" +
		"        int x = 1; // System.currentTimeMillis()\n" +
		"        return x;\n" +
		"    }\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "Main.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runVetJava(dir); code != 0 {
		t.Errorf("a determinism finding was reported for a file whose only mention " +
			"of a forbidden API is in a trailing comment.\n\n" +
			"javaCodeOnly is not reached: TestJavaCodeOnly exercises it directly " +
			"and passes either way, so this arm is the one that notices when the " +
			"scan stops calling it. cleat#1820.")
	}

	// FLOOR. Every assertion above is also satisfied by a vet that found no
	// files, or refused to run at all -- "0 findings" and "0 examined" look
	// identical from out here. A real violation in the same project must still
	// be reported.
	if err := os.WriteFile(filepath.Join(dir, "Real.java"),
		[]byte("public class Real {\n    public static long t() { return System.currentTimeMillis(); }\n}\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if code := runVetJava(dir); code == 0 {
		t.Errorf("a real determinism violation was NOT reported.\n\n" +
			"The arm above therefore proves nothing: it would pass equally well " +
			"if the scan had stopped examining anything.")
	}
}
