package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheJavaScanSkipsTestSources asserts that the determinism checker reads
// production sources and not test source sets — and, in the same run, that
// narrowing it has not stopped it reading anything real.
//
// # Why this matters more than it used to
//
// While `cleat vet --lang java` was opt-in, scanning a test file was noise.
// #1791 wired runVetJava into `cleat build --target java`, so a non-zero exit
// now REFUSES THE BUILD. A test that reads a fixture file is not a determinism
// defect — reading a fixture file is what tests are for — and refusing a build
// over one is a false positive on the first project that has an integration
// test. cleat#1789.
//
// # A narrowing change asks the opposite question of a widening one
//
// This change only REMOVES files from the scan, so it cannot produce a false
// positive; it can only stop reporting something real. So the arms below are
// weighted accordingly: one asserts the exclusion works, and three assert that
// coverage did not leak away with it. The distinction is WS-3's, from the Rust
// twin (#1815), and it is the right way round for this kind of edit.
//
// # Why the rule is a PAIR and not a directory name
//
// Matching any directory called "test" would also skip a `test` PACKAGE inside
// main sources — `src/main/java/com/foo/test/` is ordinary Java, and its
// contents are compiled into the artifact. The rule is therefore "a directory
// named test or testFixtures whose PARENT is named src", which is Gradle and
// Maven source-set layout and still holds for a multi-module project where the
// path is <module>/src/test.
func TestTheJavaScanSkipsTestSources(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	const violation = `public class T {
    public static long t() { return System.currentTimeMillis(); }
}
`

	// A project with every source set present, and the violation placed in
	// exactly one of them per subtest.
	newProject := func(t *testing.T, where string) string {
		t.Helper()
		dir := t.TempDir()
		for _, sub := range []string{
			"src/main/java", "src/test/java", "src/testFixtures/java", "app/src/test/java",
			"src/main/java/com/foo/test",
		} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "build.gradle"),
			[]byte("plugins { id 'java' }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A clean production file, so the project is never empty -- a scan
		// that found no files at all would exit non-zero for that reason and
		// every arm below would be measuring the wrong thing.
		if err := os.WriteFile(filepath.Join(dir, "src/main/java/Main.java"),
			[]byte("public class Main {\n    public static int workflow() { return 1; }\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, where, "T.java"), []byte(violation), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	excluded := []struct{ name, path string }{
		{"src/test", "src/test/java"},
		{"src/testFixtures", "src/testFixtures/java"},
		{"a multi-module <module>/src/test", "app/src/test/java"},
	}
	for _, tc := range excluded {
		t.Run("a violation in "+tc.name+" does not refuse the build", func(t *testing.T) {
			if code := runVetJava(newProject(t, tc.path)); code != 0 {
				t.Errorf("a determinism violation in %s was reported.\n\n"+
					"Test sources are not compiled into the artifact, so their "+
					"determinism is not a property of anything that replays. Since "+
					"#1791 this refuses the build, which means a test that reads a "+
					"fixture file stops the project compiling.", tc.path)
			}
		})
	}

	// The three arms that matter for a narrowing change: did coverage leak?
	kept := []struct{ name, path, why string }{
		{"src/main", "src/main/java",
			"production sources are the whole point of the check"},
		{"a test PACKAGE inside main sources", "src/main/java/com/foo/test",
			"its parent is 'foo', not 'src', so it is ordinary Java that IS compiled " +
				"into the artifact -- this is why the rule matches the src/test PAIR " +
				"rather than any directory called test"},
	}
	for _, tc := range kept {
		t.Run("a violation in "+tc.name+" is still reported", func(t *testing.T) {
			if code := runVetJava(newProject(t, tc.path)); code == 0 {
				t.Errorf("a determinism violation in %s was NOT reported.\n\n"+
					"The exclusion has taken coverage with it: %s", tc.path, tc.why)
			}
		})
	}

	t.Run("the exclusion is not what makes the scan pass", func(t *testing.T) {
		// A floor assertion. Every arm above would also pass if runVetJava had
		// stopped finding any .java file at all -- "0 problems" and "0 examined"
		// look identical from the outside.
		dir := newProject(t, "src/test/java")
		out, err := os.ReadDir(filepath.Join(dir, "src/main/java"))
		if err != nil || len(out) == 0 {
			t.Fatalf("the fixture has no production sources, so the exclusion arms prove nothing")
		}
		var names []string
		for _, e := range out {
			names = append(names, e.Name())
		}
		if !strings.Contains(strings.Join(names, " "), "Main.java") {
			t.Fatalf("expected a production source in the fixture, got %v", names)
		}
	})
}
