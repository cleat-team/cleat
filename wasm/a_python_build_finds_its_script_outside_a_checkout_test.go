package wasm

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#3793. `cleat build --target python` found build_wasm.py only by walking
// up to a cleat repo root, so a project outside a checkout failed with
// "finding repo root: go.mod not found". These pin the order of the two lookups
// and what each failure says, without a Python toolchain: the lookups are
// passed in, which is what locatePythonBuildScript is split out for.

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installedAt(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func notInstalled() (string, error) { return "", errors.New("import cleat_sdk finds nothing") }

func TestPythonBuildScript_ACheckoutWinsAndTheInstalledPackageIsNotAsked(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "python-sdk", "scripts", "build_wasm.py"))
	asked := false

	script, dir, err := locatePythonBuildScript(
		func(string) (string, error) { return root, nil },
		func() (string, error) { asked = true; return "", errors.New("unused") })
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "python-sdk", "scripts", "build_wasm.py"); script != want {
		t.Errorf("script = %q, want the checkout's %q", script, want)
	}
	if want := filepath.Join(root, "python-sdk"); dir != want {
		t.Errorf("workDir = %q, want the SDK root %q, which is where the script has always run", dir, want)
	}
	if asked {
		t.Error("the installed package was consulted although a checkout had the script")
	}
}

func TestPythonBuildScript_OutsideACheckoutTheInstalledPackageIsUsed(t *testing.T) {
	pkg := t.TempDir()
	touch(t, filepath.Join(pkg, "_build", "build_wasm.py"))

	script, dir, err := locatePythonBuildScript(
		func(string) (string, error) { return "", errors.New("go.mod not found from .") },
		installedAt(pkg))
	if err != nil {
		t.Fatalf("a project outside a checkout, with cleat-sdk installed, must build: %v", err)
	}
	if want := filepath.Join(pkg, "_build", "build_wasm.py"); script != want {
		t.Errorf("script = %q, want %q", script, want)
	}
	if dir != "" {
		t.Errorf("workDir = %q, want \"\": an installed SDK has no root to run in, and every path the script gets is absolute", dir)
	}
}

// FindRepoRoot falls back to the nearest go.mod of any kind, so a Go project
// that happens to hold a Python workflow resolves to ITS root, which has no
// python-sdk. That used to end in "build script not found at <their>/python-sdk".
func TestPythonBuildScript_SomeOtherProjectsGoModDoesNotHideTheInstalledPackage(t *testing.T) {
	other := t.TempDir() // a go.mod project, no python-sdk beneath it
	pkg := t.TempDir()
	touch(t, filepath.Join(pkg, "_build", "build_wasm.py"))

	script, _, err := locatePythonBuildScript(
		func(string) (string, error) { return other, nil }, installedAt(pkg))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(script, pkg) {
		t.Errorf("script = %q, want one under the installed package %q", script, pkg)
	}
}

func TestPythonBuildScript_NothingInstalledSaysHowToInstallIt(t *testing.T) {
	_, _, err := locatePythonBuildScript(
		func(string) (string, error) { return "", errors.New("go.mod not found from .") }, notInstalled)
	if err == nil {
		t.Fatal("no checkout and no installed SDK must be an error")
	}
	if !IsPythonBuildScriptNotFound(err) {
		t.Errorf("%v is not recognised as a missing build script, so the CLI would add compile-failure advice to it", err)
	}
	for _, want := range []string{"not inside a cleat checkout", "pip install cleat-sdk", "venv"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

// 0.5.0 on PyPI is installed and importable and has no _build directory. The
// message has to say that, not "not installed".
func TestPythonBuildScript_AnOlderInstalledSDKIsToldToUpgrade(t *testing.T) {
	old := t.TempDir() // cleat_sdk present, no _build/
	_, _, err := locatePythonBuildScript(
		func(string) (string, error) { return "", errors.New("go.mod not found from .") }, installedAt(old))
	if err == nil {
		t.Fatal("an installed SDK without the build script must be an error")
	}
	if !IsPythonBuildScriptNotFound(err) {
		t.Errorf("%v is not recognised as a missing build script", err)
	}
	for _, want := range []string{old, "has no build script", "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "not installed") {
		t.Errorf("error %q calls an installed package not installed", err)
	}
}

// The other half of the contract is python-sdk/setup.py, which puts the script
// where this looks. A rename on either side alone would ship a wheel the CLI
// cannot use, and nothing but a real build would notice.
func TestPythonBuildScript_SetupPyShipsTheScriptWhereTheCLILooksForIt(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "python-sdk", "setup.py"))
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Dir(pythonBuildScriptRel)
	re := regexp.MustCompile(`\("scripts",\s*"build_wasm\.py",\s*"` + regexp.QuoteMeta(dest) + `"\)`)
	if !re.Match(src) {
		t.Errorf("python-sdk/setup.py has no SHIPPED entry (\"scripts\", \"build_wasm.py\", %q); "+
			"the CLI looks for %s inside the installed package", dest, pythonBuildScriptRel)
	}
}
