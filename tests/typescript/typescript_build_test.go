package typescript

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// These tests run packages/cleat-ts/build.sh, which needs the TypeScript toolchain (the pinned javy and esbuild from
// packages/cleat-ts/bootstrap.sh, cargo with wasm32-wasip1, python3, go). They live in this package, which is tier 2 and in
// no tier-1 pattern, so a missing toolchain can never be a tier-1 failure.
//
// Absent toolchain: a SKIP, unless CLEAT_REQUIRE_TOOLCHAINS names "typescript", in which case it is a failure. The job
// that provisions the toolchain sets that, so a setup that silently stopped installing it cannot turn every test here
// into a skip. Deliberately not keyed on the workflow's display name (cleat IMPROVEMENT-PLAN 2.9).
const requireToolchainEnv = "CLEAT_REQUIRE_TOOLCHAINS"

func toolchainRequired(name string) bool {
	for _, want := range strings.Split(os.Getenv(requireToolchainEnv), ",") {
		if strings.TrimSpace(want) == name {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.HasPrefix(string(b), "module github.com/cleat-team/cleat\n") {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no repository root above %s", dir)
		}
	}
}

// needToolchain skips (or fails, when the job declared the toolchain) unless bootstrap.sh has run and the Rust target exists.
func needToolchain(t *testing.T) (root string) {
	t.Helper()
	root = repoRoot(t)
	missing := func(why string) {
		t.Helper()
		if toolchainRequired("typescript") {
			t.Fatalf("%s, but %s declares typescript, so this job installs the TypeScript toolchain: the setup step is broken, not the test", why, requireToolchainEnv)
		}
		t.Skip(why + " (run packages/cleat-ts/bootstrap.sh; only the TypeScript tier-2 job provisions this)")
	}
	for _, rel := range []string{".tools/javy", "node_modules/.bin/esbuild"} {
		if _, err := os.Stat(filepath.Join(root, "packages", "cleat-ts", rel)); err != nil {
			missing("packages/cleat-ts/" + rel + " is missing")
		}
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		missing("cargo is not installed")
	}
	if out, err := exec.Command("rustup", "target", "list", "--installed").Output(); err != nil || !strings.Contains(string(out), "wasm32-wasip1") {
		missing("the Rust target wasm32-wasip1 is not installed")
	}
	return root
}

// buildResult is what build.sh did: its exit code, its combined output, and the module it wrote (empty on failure).
type buildResult struct {
	exit   int
	output string
	module string
}

func build(t *testing.T, root, src, entries string, env ...string) buildResult {
	t.Helper()
	outDir := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "packages", "cleat-ts", "build.sh"), src, entries)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), append([]string{"OUT_DIR=" + outDir}, env...)...)
	out, err := cmd.CombinedOutput()
	res := buildResult{output: string(out)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.exit = ee.ExitCode()
	default:
		t.Fatalf("could not run build.sh: %v", err)
	}
	if res.exit == 0 {
		name := strings.TrimSuffix(filepath.Base(src), ".ts")
		if e := os.Getenv("OUT_NAME"); e != "" {
			name = e
		}
		res.module = filepath.Join(outDir, name+".wasm")
	}
	return res
}

func sha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestTypeScriptBuildProducesAModuleTheEngineRoutes is the positive control for everything below: the pinned chain
// builds a workflow, and the module it writes declares the label the engine routes to wasmtime. Without it the refusal
// tests are satisfied by a build.sh that refuses everything.
func TestTypeScriptBuildProducesAModuleTheEngineRoutes(t *testing.T) {
	root := needToolchain(t)
	res := build(t, root, "tests/typescript/testdata/workflows/hello.ts", "hello")
	if res.exit != 0 {
		t.Fatalf("build.sh exited %d:\n%s", res.exit, res.output)
	}
	b, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("the module is empty")
	}
	if got := wasm.DetectLanguage(b); got != "javascript" {
		t.Errorf("wasm.DetectLanguage(module) = %q, want %q", got, "javascript")
	}
	if !engine.RunsOnWasmtime("javascript") {
		t.Error(`the module declares "javascript" but the engine does not route that label to wasmtime`)
	}
	md, err := wasm.ReadMetadata(b)
	if err != nil {
		t.Fatalf("the module carries no readable cleat metadata: %v", err)
	}
	if md.Language != "javascript" {
		t.Errorf("metadata language = %q, want %q", md.Language, "javascript")
	}
	t.Logf("module size: %d bytes", len(b))
}

// TestTypeScriptBuildIsDeterministicFromOneDirectory pins that identical source gives identical bytes from one directory.
// Where the source lives is the next test's question (cleat#3833).
func TestTypeScriptBuildIsDeterministicFromOneDirectory(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/sleeper.ts"
	a := build(t, root, src, "sleeper")
	b := build(t, root, src, "sleeper")
	if a.exit != 0 || b.exit != 0 {
		t.Fatalf("builds failed (exit %d, %d):\n%s\n%s", a.exit, b.exit, a.output, b.output)
	}
	if sa, sb := sha(t, a.module), sha(t, b.module); sa != sb {
		t.Errorf("two builds of one source differ: %s vs %s", sa, sb)
	}
}

// TestTypeScriptBuildIsIndependentOfWhereTheSourceLives is cleat#3833: the same source built from two directories gave two
// different modules, because esbuild writes a `// <path>` comment for each file it bundles, relative to the toolchain
// directory, and Javy embeds the whole bundle. For a durable engine that stamps and compares a module's identity that is a
// defect: the same workflow built on two machines, or in a checkout at another path, must be the same module.
//
// The three locations differ in depth and in name, and one is inside the repository. The depth matters because the leaked
// path is a run of `../` whose length is the distance from the toolchain directory; the in-repo copy is the case that
// hid the defect, since its path is the same on every build.
func TestTypeScriptBuildIsIndependentOfWhereTheSourceLives(t *testing.T) {
	root := needToolchain(t)
	body, err := os.ReadFile(filepath.Join(root, "tests", "typescript", "testdata", "workflows", "sleeper.ts"))
	if err != nil {
		t.Fatal(err)
	}
	shallow := t.TempDir()
	deep := filepath.Join(t.TempDir(), "a-much-longer-directory-name", "and", "another", "level")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	var srcs []string
	for _, dir := range []string{shallow, deep} {
		src := filepath.Join(dir, "sleeper.ts")
		if err := os.WriteFile(src, body, 0o644); err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, src)
	}
	srcs = append(srcs, "tests/typescript/testdata/workflows/sleeper.ts")

	want := ""
	for _, src := range srcs {
		res := build(t, root, src, "sleeper")
		if res.exit != 0 {
			t.Fatalf("build of %s failed (exit %d):\n%s", src, res.exit, res.output)
		}
		got := sha(t, res.module)
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Errorf("the same source built from %s differs from the one built from %s:\n  %s\n  %s\nthe module embeds where the source lives",
				src, srcs[0], got, want)
		}
	}
}

// TestTypeScriptBuildIsIndependentOfWhereAnImportLives is the same property for what a workflow IMPORTS from outside its
// own directory, the case a monorepo has: a sibling directory the source root does not cover, and a package hoisted into a
// node_modules above it. Each is labelled without its location (external/helper.ts, node_modules/leftpad/index.js), so the
// project can be checked out anywhere, at any depth, and build the same module.
func TestTypeScriptBuildIsIndependentOfWhereAnImportLives(t *testing.T) {
	root := needToolchain(t)
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project := func(base string) string {
		write(filepath.Join(base, "proj", "app", "wf.ts"),
			"import { pad } from \"../shared/helper\";\nimport leftpad from \"leftpad\";\n"+
				"declare function __cleat_result(json: string): void;\n"+
				"export function ext(): void {\n  __cleat_result(JSON.stringify({ a: pad(\"x\"), b: leftpad(\"y\") }));\n}\n")
		write(filepath.Join(base, "proj", "shared", "helper.ts"),
			"export function pad(s: string): string {\n  return \"[\" + s + \"]\";\n}\n")
		write(filepath.Join(base, "proj", "node_modules", "leftpad", "package.json"),
			"{\"name\":\"leftpad\",\"version\":\"1.0.0\",\"main\":\"index.js\"}\n")
		write(filepath.Join(base, "proj", "node_modules", "leftpad", "index.js"),
			"module.exports = function (s) {\n  return \" \" + s;\n};\n")
		return filepath.Join(base, "proj", "app", "wf.ts")
	}
	a := build(t, root, project(t.TempDir()), "ext")
	b := build(t, root, project(filepath.Join(t.TempDir(), "a-much-longer-directory-name", "and", "another", "level")), "ext")
	if a.exit != 0 || b.exit != 0 {
		t.Fatalf("builds failed (exit %d, %d):\n%s\n%s", a.exit, b.exit, a.output, b.output)
	}
	if sa, sb := sha(t, a.module), sha(t, b.module); sa != sb {
		t.Errorf("the same project built at two depths differs: %s vs %s\nan import from outside the source root embeds where it lives", sa, sb)
	}
}

// TestTypeScriptStaticCheckRefusesATimerAndNamesTheLine builds a workflow that calls setTimeout. The build must stop with
// exit 1 (a finding), say which file and line, and write no module: a timer is not a durable operation.
func TestTypeScriptStaticCheckRefusesATimerAndNamesTheLine(t *testing.T) {
	root := needToolchain(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "timer.ts")
	body := "export function timer(): void {\n  setTimeout(() => {}, 10);\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res := build(t, root, src, "timer")
	if res.exit != 1 {
		t.Fatalf("exit = %d, want 1 (a finding):\n%s", res.exit, res.output)
	}
	if !strings.Contains(res.output, "timer.ts:2:") || !strings.Contains(res.output, "setTimeout") {
		t.Errorf("the refusal does not name the file, line and global:\n%s", res.output)
	}
	if res.module != "" {
		t.Errorf("a refused build still reported a module: %s", res.module)
	}
}
