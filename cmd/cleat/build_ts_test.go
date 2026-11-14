package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests cover the CLI's own logic -- argument parsing, finding the toolchain, passing the arguments through, and keeping
// the build script's exit statuses distinct -- against a FAKE build script in a FAKE toolchain directory. They run in Tier 1
// because they need no real toolchain; what the real build does is tested in tests/typescript (tier 2).

// fakeTSToolchain writes a toolchain directory whose build.sh records how it was called and exits with $FAKE_BUILD_EXIT. With
// bootstrapped, it also creates the files a bootstrapped toolchain has.
func fakeTSToolchain(t *testing.T, bootstrapped bool) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(t.TempDir(), "call.log")
	script := "#!/usr/bin/env bash\n" +
		"{ echo \"args=$*\"; echo \"OUT_DIR=$OUT_DIR\"; echo \"SRC_ROOT=$SRC_ROOT\"; for v in " + strings.Join(tsScrubbedEnv, " ") +
		"; do echo \"$v=${!v-<unset>}\"; done; } > \"$FAKE_LOG\"\n" +
		"[ -z \"${FAKE_BUILD_KILL:-}\" ] || kill -KILL $$\n" +
		"mkdir -p \"$OUT_DIR\" && : > \"$OUT_DIR/$(basename \"$1\" .ts).wasm\"\n" +
		"exit \"${FAKE_BUILD_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "build.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if bootstrapped {
		for _, rel := range tsToolchainFiles {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("CLEAT_TS_DIR", dir)
	t.Setenv("FAKE_LOG", log)
	return dir, log
}

func writeSource(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("export function f(): void {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseTSEntryAcceptsTheShapeBuildScriptDerivesFrom(t *testing.T) {
	cases := []struct {
		spec    string
		file    string
		entries []string
		bad     string // a substring of the error; empty means the spec is valid
	}{
		{spec: "src/orders.ts:place_order", file: "src/orders.ts", entries: []string{"place_order"}},
		{spec: "a.ts:one,two_words,x9", file: "a.ts", entries: []string{"one", "two_words", "x9"}},
		{spec: "src/orders.ts", bad: "expected file.ts:entry_name"},
		{spec: "src/orders.js:place_order", bad: "must be a .ts file"},
		{spec: "a.ts:placeOrder", bad: "not snake_case"},
		{spec: "a.ts:", bad: "not snake_case"},
		{spec: "a.ts:one,", bad: "not snake_case"},
		{spec: "a.ts:one__two", bad: "not snake_case"},
		{spec: "a.ts:_one", bad: "not snake_case"},
	}
	for _, c := range cases {
		file, entries, err := parseTSEntry(c.spec)
		if c.bad != "" {
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("parseTSEntry(%q) error = %v, want one containing %q", c.spec, err, c.bad)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTSEntry(%q): %v", c.spec, err)
			continue
		}
		if file != c.file || strings.Join(entries, ",") != strings.Join(c.entries, ",") {
			t.Errorf("parseTSEntry(%q) = %q, %v; want %q, %v", c.spec, file, entries, c.file, c.entries)
		}
	}
}

func TestTSBuildCallsTheScriptAndKeepsItsExitStatuses(t *testing.T) {
	_, log := fakeTSToolchain(t, true)
	work := t.TempDir()
	t.Chdir(work)
	src := writeSource(t, work, "src/orders.ts")
	out := filepath.Join(t.TempDir(), "out")

	for _, c := range []struct {
		exit     string
		want     int
		wantText string
	}{
		{"0", 0, "Wrote "},
		{"1", 1, "no artifact was emitted"}, // a finding: the source was refused
		{"2", 2, "no artifact was emitted"}, // UNMEASURED: the script could not look
	} {
		t.Setenv("FAKE_BUILD_EXIT", c.exit)
		var stderr bytes.Buffer
		if got := tsBuild("src/orders.ts:place_order", out, "latest", 1, &stderr); got != c.want {
			t.Errorf("script exit %s: tsBuild = %d, want %d\n%s", c.exit, got, c.want, stderr.String())
		}
		if !strings.Contains(stderr.String(), c.wantText) {
			t.Errorf("script exit %s: output lacks %q:\n%s", c.exit, c.wantText, stderr.String())
		}
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	realSrc, _ := filepath.EvalSymlinks(src)
	got := string(b)
	for _, want := range []string{"args=" + src + " place_order", "OUT_DIR=" + out} {
		if !strings.Contains(got, want) {
			t.Errorf("the script was not called with %q:\n%s", want, got)
		}
	}
	// The source is under the working directory, so labels are relative to it (src/orders.ts), not to the source's directory.
	if !strings.Contains(got, "SRC_ROOT="+work) && !strings.Contains(got, "SRC_ROOT="+filepath.Dir(filepath.Dir(realSrc))) {
		t.Errorf("SRC_ROOT is not the working directory %s:\n%s", work, got)
	}
}

// A status other than 0, 1 or 2, and death by a signal, are neither a finding nor a build: they are a script that did not run to
// the end (an OOM kill is 137, a missing interpreter 127). Passed through, 137 would be read by a caller as its own code, and a
// signal would be reported as exit -1. Both are UNMEASURED, which is 2.
func TestTSBuildReportsAnyOtherExitStatusOrASignalAsUNMEASURED(t *testing.T) {
	fakeTSToolchain(t, true)
	work := t.TempDir()
	t.Chdir(work)
	writeSource(t, work, "a.ts")
	for _, c := range []struct{ name, exit, kill string }{
		{"exit 3", "3", ""},
		{"exit 127 (interpreter not found)", "127", ""},
		{"exit 137 (OOM-killed)", "137", ""},
		{"killed by a signal", "0", "1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FAKE_BUILD_EXIT", c.exit)
			t.Setenv("FAKE_BUILD_KILL", c.kill)
			var stderr bytes.Buffer
			if got := tsBuild("a.ts:f", filepath.Join(work, "out"), "latest", 1, &stderr); got != 2 {
				t.Fatalf("tsBuild = %d, want 2 (UNMEASURED)\n%s", got, stderr.String())
			}
			if !strings.Contains(stderr.String(), "UNMEASURED") {
				t.Errorf("the output does not say UNMEASURED:\n%s", stderr.String())
			}
		})
	}
}

// The module must be a function of the source and the toolchain, not of the caller's environment. build.sh reads OUT_NAME and
// several development controls (a shim switched off, a fault injected, the determinism flag); one left in a shell would change
// the module with nothing on the command line to show it.
func TestTSBuildDoesNotPassTheCallersBuildControlsToTheScript(t *testing.T) {
	_, log := fakeTSToolchain(t, true)
	work := t.TempDir()
	t.Chdir(work)
	writeSource(t, work, "a.ts")
	for _, v := range tsScrubbedEnv {
		t.Setenv(v, "stray")
	}
	var stderr bytes.Buffer
	if got := tsBuild("a.ts:f", filepath.Join(work, "out"), "latest", 1, &stderr); got != 0 {
		t.Fatalf("tsBuild = %d\n%s", got, stderr.String())
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range tsScrubbedEnv {
		if !strings.Contains(string(b), v+"=<unset>\n") {
			t.Errorf("%s reached the build script:\n%s", v, b)
		}
	}
	// The control: the variables the command sets on purpose do arrive, so "<unset>" above is the scrub and not a broken log.
	if !strings.Contains(string(b), "OUT_DIR="+filepath.Join(work, "out")) {
		t.Errorf("OUT_DIR did not arrive, so the log proves nothing:\n%s", b)
	}
}

// TestTSScrubListCoversEveryBuildControlBuildShReads checks the scrub list against two readings that do not come from the list
// itself. The test above sets and checks whatever tsScrubbedEnv says, so it agrees with the list by construction: a control missing
// from the list is invisible to it.
//
//  1. A literal set of the build controls, written here. It includes the two the plugin crate reads (they never appear in
//     build.sh's code, only in its header comment), so parsing the script could not find them.
//  2. Every upper-case variable build.sh itself references. Each must be scrubbed or classified below as something that is not a
//     build control. A variable added to build.sh later fails here until someone decides which it is.
func TestTSScrubListCoversEveryBuildControlBuildShReads(t *testing.T) {
	scrubbed := map[string]bool{}
	for _, v := range tsScrubbedEnv {
		scrubbed[v] = true
	}
	for _, v := range []string{"OUT_NAME", "JAVY_DETERMINISTIC", "SOURCEMAP", "NAN_POLICY", "SUSPEND_MODE", "RESULT_CANONICAL",
		"PLUGIN", "CLEAT_SHIMS_OFF", "CLEAT_FAULT"} {
		if !scrubbed[v] {
			t.Errorf("%s changes the module and is not in tsScrubbedEnv", v)
		}
	}

	notAControl := map[string]string{
		"OUT_DIR": "set by tsBuild", "SRC_ROOT": "set by tsBuild",
		"REBUILD_PLUGIN": "only forces a rebuild of the shipped plugin",
		"HOME":           "plumbing", "CARGO_HOME": "plumbing", "RUSTFLAGS": "plumbing", "NODE_PATH": "plumbing", "BASH_SOURCE": "plumbing",
		"HERE": "local", "ROOT": "local", "W": "local", "J": "local", "E": "local", "LINK_DIR": "local", "PLUGIN_DIR": "local", "SRC_ABS": "local",
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "packages", "cleat-ts", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)|os\.environ\.get\("([A-Z][A-Z0-9_]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(refs) < 10 {
		t.Fatalf("found only %d variable references in build.sh, so the pattern is not reading it", len(refs))
	}
	seen := map[string]bool{}
	for _, m := range refs {
		v := m[1] + m[2]
		if seen[v] {
			continue
		}
		seen[v] = true
		if !scrubbed[v] && notAControl[v] == "" {
			t.Errorf("build.sh reads %s, which is neither scrubbed nor classified as not a build control", v)
		}
	}
}

func TestTSBuildRefusesAnUnbootstrappedToolchainAsUNMEASURED(t *testing.T) {
	// The positive control first: the same call against a bootstrapped toolchain builds, so the refusal below is the missing
	// files and not something the fake does to every call.
	_, _ = fakeTSToolchain(t, true)
	work := t.TempDir()
	t.Chdir(work)
	writeSource(t, work, "a.ts")
	var ok bytes.Buffer
	if got := tsBuild("a.ts:f", filepath.Join(work, "out"), "latest", 1, &ok); got != 0 {
		t.Fatalf("control: a bootstrapped toolchain did not build (exit %d):\n%s", got, ok.String())
	}

	dir, _ := fakeTSToolchain(t, false)
	var stderr bytes.Buffer
	got := tsBuild("a.ts:f", filepath.Join(work, "out2"), "latest", 1, &stderr)
	if got != 2 {
		t.Fatalf("an unbootstrapped toolchain: exit = %d, want 2 (UNMEASURED)\n%s", got, stderr.String())
	}
	msg := stderr.String()
	for _, want := range []string{"UNMEASURED", tsToolchainFiles[0], tsToolchainFiles[1], filepath.Join(dir, "bootstrap.sh"), "not a finding about a.ts"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, msg)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "out2")); err == nil {
		t.Errorf("an UNMEASURED build still created its output directory")
	}
}

func TestTSBuildRefusesAMalformedOrMissingSourceAsAFinding(t *testing.T) {
	fakeTSToolchain(t, true)
	work := t.TempDir()
	t.Chdir(work)
	for _, spec := range []string{"no-entry.ts", "missing.ts:f", "a.ts:BadName"} {
		var stderr bytes.Buffer
		if got := tsBuild(spec, work, "latest", 1, &stderr); got != 1 {
			t.Errorf("tsBuild(%q) = %d, want 1\n%s", spec, got, stderr.String())
		}
	}
}

func TestTSSourceRootIsTheWorkingDirectoryOnlyWhenTheSourceIsUnderIt(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	inside := writeSource(t, work, "src/orders.ts")
	outsideDir := t.TempDir()
	outside := writeSource(t, outsideDir, "orders.ts")

	if got := tsSourceRoot(inside); got != work {
		t.Errorf("a source under the working directory: root = %s, want %s", got, work)
	}
	if got := tsSourceRoot(outside); got != outsideDir {
		t.Errorf("a source outside the working directory: root = %s, want its own directory %s", got, outsideDir)
	}
	// A sibling whose name merely starts with the working directory's must not count as under it.
	sibling := writeSource(t, work+"-sibling", "x.ts")
	t.Cleanup(func() { _ = os.RemoveAll(work + "-sibling") })
	if got := tsSourceRoot(sibling); got != work+"-sibling" {
		t.Errorf("a sibling directory sharing a name prefix: root = %s, want %s", got, work+"-sibling")
	}
}

func TestFindTSToolchain(t *testing.T) {
	t.Run("the environment variable wins, and must name a directory with build.sh", func(t *testing.T) {
		dir, _ := fakeTSToolchain(t, false)
		got, err := findTSToolchain()
		if err != nil || got != dir {
			t.Fatalf("findTSToolchain() = %q, %v; want %q", got, err, dir)
		}
		t.Setenv("CLEAT_TS_DIR", t.TempDir())
		if _, err := findTSToolchain(); err == nil || !strings.Contains(err.Error(), "has no build.sh") {
			t.Errorf("a directory without build.sh: error = %v", err)
		}
	})
	t.Run("without it, the nearest ancestor of the working directory", func(t *testing.T) {
		t.Setenv("CLEAT_TS_DIR", "")
		root := t.TempDir()
		tc := filepath.Join(root, tsToolchainSubdir)
		if err := os.MkdirAll(tc, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tc, "build.sh"), []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		deep := filepath.Join(root, "some", "project", "dir")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(deep)
		got, err := findTSToolchain()
		if err != nil {
			t.Fatal(err)
		}
		// Real paths: a macOS temp directory is reached through a symlink.
		realGot, _ := filepath.EvalSymlinks(got)
		realWant, _ := filepath.EvalSymlinks(tc)
		if realGot != realWant {
			t.Errorf("findTSToolchain() = %s, want %s", realGot, realWant)
		}
	})
}

func TestTSIsAValidBuildTarget(t *testing.T) {
	if !isValidTarget("ts") {
		t.Error(`isValidTarget("ts") = false`)
	}
	if isValidTarget("typescript") || isValidTarget("js") {
		t.Error("only the target name ts is accepted: the other spellings would each need their own documentation")
	}
}
