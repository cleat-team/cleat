package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// `cleat build --target csharp` (cleat#4080, C# PR 2). These tests run WITHOUT docker or the .NET toolchain: CLEAT_CSHARP_DIR points at a
// fake packages/cleat-csharp whose build.sh plays one scenario, so what is tested is everything the CLI does around the container:
// finding the toolchain, mapping the script's exit status, reading the entry-point list strictly, and the stamp. The container build
// itself is tests/csharp/csharp_build_test.go.

// componentHeader is the smallest thing wasm.WriteMetadata accepts as a Component Model binary: the magic and the component layer
// version (0x0d 0x00 0x01 0x00), no sections. The spike's real component starts the same way.
var componentHeader = []byte{0x00, 'a', 's', 'm', 0x0d, 0x00, 0x01, 0x00}

// fakeToolchain writes a packages/cleat-csharp stand-in whose build.sh does what scenario says, and returns its directory. The script
// receives the real arguments (mount-root, project-dir-relative-to-root, stage dir), exactly as the CLI passes them.
func fakeToolchain(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build.sh"), []byte("#!/usr/bin/env bash\nset -u\nstage=\"$3\"\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLEAT_CSHARP_DIR", dir)
	return dir
}

func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Hello.csproj"), []byte("<Project/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const okScript = `printf '\x00asm\x0d\x00\x01\x00' > "$stage/hello.wasm"; printf 'Hello\nSleep\n' > "$stage/entry-points.txt"; exit 0`

func build(t *testing.T, spec string, out string, version int) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := csharpBuild(spec, out, version, &buf)
	return code, buf.String()
}

func TestParseCSharpEntryPointsIsStrict(t *testing.T) {
	good, err := parseCSharpEntryPoints([]byte("Hello\nSleep\nDefers\n"))
	if err != nil || strings.Join(good, ",") != "Hello,Sleep,Defers" {
		t.Fatalf("a valid list: got %v, %v; want [Hello Sleep Defers] in file order", good, err)
	}
	for name, in := range map[string]string{
		"empty":             "",
		"a UTF-8 BOM":       "\xef\xbb\xbfHello\n",
		"CRLF":              "Hello\r\nSleep\r\n",
		"no final newline":  "Hello\nSleep",
		"a blank line":      "Hello\n\nSleep\n",
		"a duplicate":       "Hello\nSleep\nHello\n",
		"a space in a name": "Hello World\n",
		"a comma in a name": "Hello,Sleep\n",
		"a tab in a name":   "Hel\tlo\n",
		"only a newline":    "\n",
		"a BOM mid-file":    "Hello\n\xef\xbb\xbfSleep\n",
	} {
		if got, err := parseCSharpEntryPoints([]byte(in)); err == nil {
			t.Errorf("%s was accepted: %q -> %v", name, in, got)
		}
	}
	// The BOM error says what is wrong, so a person can fix the writer rather than guess.
	if _, err := parseCSharpEntryPoints([]byte("\xef\xbb\xbfHello\n")); err == nil || !strings.Contains(err.Error(), "byte-order mark") {
		t.Errorf("the BOM refusal does not name the BOM: %v", err)
	}
}

func TestACSharpBuildStampsTheComponentAndWritesIt(t *testing.T) {
	fakeToolchain(t, okScript)
	out := t.TempDir()
	code, msg := build(t, project(t), out, 3)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, msg)
	}
	b, err := os.ReadFile(filepath.Join(out, "hello.wasm"))
	if err != nil {
		t.Fatalf("no artifact: %v\n%s", err, msg)
	}
	meta, err := wasm.ReadMetadata(b)
	if err != nil {
		t.Fatalf("the artifact carries no cleat.metadata: %v", err)
	}
	if meta.Language != "csharp" || meta.WorkflowName != "hello" || meta.WorkflowVersion != 3 ||
		strings.Join(meta.EntryPoints, ",") != "Hello,Sleep" {
		t.Errorf("stamp = %+v, want language csharp, name hello, version 3, entry points [Hello Sleep]", meta)
	}
	if err := meta.Validate(); err != nil {
		t.Errorf("the stamp does not pass Validate(), which `cleat deploy` runs: %v", err)
	}
	// The reason the stamp is mandatory: detection answers csharp WITH it, and the unstamped header alone is not C#.
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Errorf("DetectLanguage(stamped) = %q, want csharp", got)
	}
	if got := wasm.DetectLanguage(componentHeader); got == "csharp" {
		t.Errorf("DetectLanguage(unstamped) = %q: the control is meaningless if the unstamped component already reads as csharp", got)
	}
	if !strings.Contains(msg, "not supported") {
		t.Errorf("the build does not say C# is not supported:\n%s", msg)
	}
}

func TestACSharpBuildMapsTheScriptsExitStatusAndWritesNothingOnAFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   int
	}{
		{"the project was refused (1)", `echo 'error CS1002' >&2; exit 1`, 1},
		{"UNMEASURED (2)", `echo 'UNMEASURED: no docker' >&2; exit 2`, 2},
		{"a status that is neither (7) is UNMEASURED, not a finding", `exit 7`, 2},
		{"docker's own failure (125) is UNMEASURED", `exit 125`, 2},
		{"killed by a signal is UNMEASURED", `kill -9 $$`, 2},
		{"success with no .wasm", `printf 'Hello\n' > "$stage/entry-points.txt"; exit 0`, 2},
		{"success with two .wasm", `printf 'x' > "$stage/a.wasm"; printf 'x' > "$stage/b.wasm"; printf 'Hello\n' > "$stage/entry-points.txt"; exit 0`, 2},
		{"success with no entry-point list", `printf '\x00asm\x0d\x00\x01\x00' > "$stage/hello.wasm"; exit 0`, 1},
		{"success with an entry-point list that has a BOM", `printf '\x00asm\x0d\x00\x01\x00' > "$stage/hello.wasm"; printf '\xef\xbb\xbfHello\n' > "$stage/entry-points.txt"; exit 0`, 1},
		{"success with an empty entry-point list", `printf '\x00asm\x0d\x00\x01\x00' > "$stage/hello.wasm"; : > "$stage/entry-points.txt"; exit 0`, 1},
		{"success with something that is not a wasm module, which cannot be stamped", `printf 'not wasm' > "$stage/hello.wasm"; printf 'Hello\n' > "$stage/entry-points.txt"; exit 0`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeToolchain(t, tc.script)
			out := t.TempDir()
			code, msg := build(t, project(t), out, 1)
			if code != tc.want {
				t.Errorf("exit %d, want %d:\n%s", code, tc.want, msg)
			}
			if entries, _ := os.ReadDir(out); len(entries) != 0 {
				t.Errorf("a failed build left %d file(s) in the output directory (an artifact must exist only if every step succeeded)", len(entries))
			}
		})
	}
}

func TestACSharpBuildRefusesWhatItCannotBuild(t *testing.T) {
	out := t.TempDir()
	fakeToolchain(t, okScript)

	if code, msg := build(t, "", out, 1); code != 1 {
		t.Errorf("no project: exit %d, want 1:\n%s", code, msg)
	}
	if code, msg := build(t, filepath.Join(t.TempDir(), "nope"), out, 1); code != 1 {
		t.Errorf("a project that does not exist: exit %d, want 1:\n%s", code, msg)
	}
	two := t.TempDir()
	os.WriteFile(filepath.Join(two, "A.csproj"), []byte("<Project/>"), 0o644)
	os.WriteFile(filepath.Join(two, "B.csproj"), []byte("<Project/>"), 0o644)
	if code, msg := build(t, two, out, 1); code != 1 || !strings.Contains(msg, "exactly one .csproj") {
		t.Errorf("two projects: exit %d, want 1 naming the rule:\n%s", code, msg)
	}
	if code, msg := build(t, project(t)+"/Hello.csproj", out, 1); code != 0 {
		t.Errorf("a path to the .csproj itself: exit %d, want 0:\n%s", code, msg)
	}
	// An environment without the toolchain is UNMEASURED, never a finding about the project.
	t.Setenv("CLEAT_CSHARP_DIR", filepath.Join(t.TempDir(), "absent"))
	if code, msg := build(t, project(t), out, 1); code != 2 || !strings.Contains(msg, "UNMEASURED") {
		t.Errorf("no toolchain: exit %d, want 2 saying UNMEASURED:\n%s", code, msg)
	}
}

func TestTheCSharpScriptReceivesTheMountRootTheProjectAndAStageDirectory(t *testing.T) {
	fakeToolchain(t, `printf '%s\n%s\n' "$1" "$2" > "$stage/args.txt"; printf '\x00asm\x0d\x00\x01\x00' > "$stage/hello.wasm"; printf 'Hello\n' > "$stage/entry-points.txt"; cp "$stage/args.txt" "${ARGS_OUT}"; exit 0`)
	argsOut := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_OUT", argsOut)
	// A repository-shaped tree: the project is two levels below a directory with a .git, which is where the WIT file and SDK sources
	// that a project refers to by relative path would be.
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	proj := filepath.Join(repo, "tests", "x")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "X.csproj"), []byte("<Project/>"), 0o644)
	if code, msg := build(t, proj, t.TempDir(), 1); code != 0 {
		t.Fatalf("exit %d:\n%s", code, msg)
	}
	got, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatal(err)
	}
	want := repo + "\n" + filepath.Join("tests", "x") + "\n"
	// macOS reports /var and /private/var for the same directory; compare real paths.
	realWant, _ := filepath.EvalSymlinks(repo)
	if !strings.HasSuffix(strings.TrimSpace(strings.SplitN(string(got), "\n", 2)[0]), filepath.Base(repo)) ||
		!strings.Contains(string(got), filepath.Join("tests", "x")) {
		t.Errorf("the script got %q, want mount root %s (%s) and project tests/x; full want %q", got, repo, realWant, want)
	}
}

func TestTheCSharpTargetIsAValidTargetAndAnEntryFlagIsRefusedForIt(t *testing.T) {
	if !isValidTarget("csharp") {
		t.Fatal(`isValidTarget("csharp") = false`)
	}
	// main() refuses --entry for csharp because --entry would silently replace the project path (it doubles as the Python pattern).
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `target == "csharp" && entry != ""`) {
		t.Error("main.go no longer refuses --entry for the csharp target: it would replace the project path with the entry value")
	}
}
