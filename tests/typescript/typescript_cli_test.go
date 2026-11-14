package typescript

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleatBuildTS runs `cleat build --target ts` and returns its exit code and combined output.
func cleatBuildTS(t *testing.T, cleatBin, dir, toolchain string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(cleatBin, append([]string{"build", "--target", "ts"}, args...)...)
	cmd.Dir = dir
	// From a directory unrelated to a checkout the command cannot find the toolchain by walking up, and says so (exit 2); the
	// documented way to point it at one is CLEAT_TS_DIR.
	cmd.Env = append(os.Environ(), "CLEAT_TS_DIR="+toolchain)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	default:
		t.Fatalf("could not run cleat: %v", err)
		return -1, ""
	}
}

// TestTypeScriptCleatBuildTargetTSProducesTheModuleTheScriptDoes is the end-to-end test of `cleat build --target ts`: the module
// the command writes is byte-identical to the one packages/cleat-ts/build.sh writes for the same source and the same source
// root, so the command adds nothing to the module. The root is the one thing the command chooses: the working directory when
// the source is under it (labels like src/orders.ts), else the source's own directory, which is also the script's default. The
// second subtest ties this to cleat#3833: a copy of the source in an unrelated directory builds to the same module as the
// repository's. The unit tests in cmd/cleat cover the command's own logic against a fake script; this is the one test that
// needs the real toolchain.
func TestTypeScriptCleatBuildTargetTSProducesTheModuleTheScriptDoes(t *testing.T) {
	root := needToolchain(t)
	cleatBin := filepath.Join(t.TempDir(), "cleat")
	goBuild(t, root, cleatBin, "./cmd/cleat")

	toolchain := filepath.Join(root, "packages", "cleat-ts")
	src := "tests/typescript/testdata/workflows/sleeper.ts"
	// What the command passes the script: the working directory (here the repository root) as the source root.
	rooted := build(t, root, src, "sleeper", "SRC_ROOT="+root)
	// The script's own default: the source's directory as the source root.
	plain := build(t, root, src, "sleeper")
	if rooted.exit != 0 || plain.exit != 0 {
		t.Fatalf("the script's own builds failed (exit %d, %d):\n%s\n%s", rooted.exit, plain.exit, rooted.output, plain.output)
	}
	wantRooted, wantPlain := sha(t, rooted.module), sha(t, plain.module)
	if wantRooted == wantPlain {
		t.Fatalf("control: the two source roots gave one module, so this test cannot tell which root the command used")
	}

	t.Run("positional file:entry, from the repository root", func(t *testing.T) {
		out := t.TempDir()
		code, text := cleatBuildTS(t, cleatBin, root, toolchain, "-o", out, src+":sleeper")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, text)
		}
		if got := sha(t, filepath.Join(out, "sleeper.wasm")); got != wantRooted {
			t.Errorf("the command's module differs from the script's with the same source root: %s vs %s", got, wantRooted)
		}
		if !strings.Contains(text, "tier-2") || !strings.Contains(text, "not supported") {
			t.Errorf("the command does not say the target is tier 2 and not supported:\n%s", text)
		}
	})

	t.Run("--entry, from an unrelated directory, on a copy of the source", func(t *testing.T) {
		body, err := os.ReadFile(filepath.Join(root, src))
		if err != nil {
			t.Fatal(err)
		}
		elsewhere := t.TempDir()
		copyPath := filepath.Join(elsewhere, "deep", "er", "sleeper.ts")
		if err := os.MkdirAll(filepath.Dir(copyPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		out := t.TempDir()
		code, text := cleatBuildTS(t, cleatBin, t.TempDir(), toolchain, "-o", out, "--entry", copyPath+":sleeper")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, text)
		}
		if got := sha(t, filepath.Join(out, "sleeper.wasm")); got != wantPlain {
			t.Errorf("a copy built from another directory differs from the repository's module: %s vs %s", got, wantPlain)
		}
	})

	t.Run("a source the static check refuses exits 1 and writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		bad := filepath.Join(dir, "timer.ts")
		if err := os.WriteFile(bad, []byte("export function timer(): void {\n  setTimeout(() => {}, 10);\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := t.TempDir()
		code, text := cleatBuildTS(t, cleatBin, dir, toolchain, "-o", out, bad+":timer")
		if code != 1 {
			t.Fatalf("exit %d, want 1 (a finding):\n%s", code, text)
		}
		if !strings.Contains(text, "timer.ts:2:") || !strings.Contains(text, "setTimeout") {
			t.Errorf("the refusal does not name the file, line and global:\n%s", text)
		}
		if _, err := os.Stat(filepath.Join(out, "timer.wasm")); err == nil {
			t.Errorf("a refused build still wrote a module")
		}
	})
}
