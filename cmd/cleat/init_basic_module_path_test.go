package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3557. `cleat init --template basic <name>` wrote the whole argument as the
// module path, so a name with a space or a quote, or an absolute path, gave a go.mod
// `go mod tidy` refuses ("usage: module module/path", "malformed module path"), and
// init still exited 0. Its own advice, `go mod tidy`, could not fix the project. The
// module is now named for the last element of the directory, as `go mod init` run
// there would, and a last element that is no import path is refused before anything
// is written.
//
// GOPROXY=off keeps `go mod tidy` from needing the network. What it cannot resolve is
// the dependency, not the module line, so the go.mod is still what init wrote, and
// `go mod edit -json` is what says whether Go can read it.

func goModOf(t *testing.T, dir string) (first string, readable bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("no go.mod in %s: %v", dir, err)
	}
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	return strings.SplitN(string(b), "\n", 2)[0], cmd.Run() == nil
}

func TestInitBasicNamesTheModuleForTheDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(base, "abs_basic")
	for _, c := range []struct{ arg, dir, module string }{
		{"ok_basic", "ok_basic", "ok_basic"},
		{"til~de_3556", "til~de_3556", "til~de_3556"},
		{"My-App.v2", "My-App.v2", "My-App.v2"},
		{"sub/dir_basic", "sub/dir_basic", "dir_basic"},
		{abs, abs, "abs_basic"},
	} {
		t.Run(c.arg, func(t *testing.T) {
			// In a child, so a refusal of a name that should be accepted reads as
			// this row failing rather than as the test binary exiting.
			out, code := initInChild(t, base, "--template", "basic", c.arg)
			if code != 0 {
				t.Fatalf("exit %d, want 0:\n%s", code, out)
			}
			dir := c.dir
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(base, dir)
			}
			first, readable := goModOf(t, dir)
			if first != "module "+c.module {
				t.Errorf("go.mod starts %q, want %q", first, "module "+c.module)
			}
			if !readable {
				t.Errorf("go mod edit cannot read the go.mod init wrote in %s", dir)
			}
		})
	}
}

// initInChild runs `init` for args in a child process, because a refusal exits and
// would end this test binary: the child is this binary, running only
// TestInitHelperProcess.
func initInChild(t *testing.T, dir string, args ...string) (stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInitHelperProcess$", "-test.short") // -test.short: TestMain would build the cleat binary
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CLEAT_INIT_HELPER=1", "GOPROXY=off", "CLEAT_INIT_ARGS="+strings.Join(args, "\x1f"))
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestInitHelperProcess(t *testing.T) {
	// Not a skip: this is the child process's entry point, which does nothing when
	// the test binary runs it as an ordinary test, and a skip would be one more
	// line in the run's skip count for a test that has nothing to skip.
	if os.Getenv("CLEAT_INIT_HELPER") != "1" {
		return
	}
	runInit(strings.Split(os.Getenv("CLEAT_INIT_ARGS"), "\x1f"))
	os.Exit(0)
}

func TestInitBasicRefusesANameThatIsNoModulePath(t *testing.T) {
	for _, name := range []string{"sp ace2_basic", "q'uote dir2", "dollar$x", "semi;colon", "sub/sp ace", "café"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadDir(dir)
			out, code := initInChild(t, dir, "--template", "basic", name)
			if code != 1 {
				t.Errorf("exit %d, want 1:\n%s", code, out)
			}
			if !strings.Contains(out, "cannot be this project's Go module path") {
				t.Errorf("the refusal does not say why:\n%s", out)
			}
			after, _ := os.ReadDir(dir)
			if len(after) != len(before) {
				t.Errorf("init left %d entries where there were %d: it must write nothing before refusing", len(after), len(before))
			}
			if _, err := os.Stat(filepath.Join(dir, "sub", "sp ace")); err == nil {
				t.Errorf("a directory was created for the refused name")
			}
		})
	}
}

// Only basic derives its module from the name; the other Go templates write a fixed
// github.com/example/... module and so scaffold any name, as they always have.
func TestInitOtherTemplatesStillTakeANameWithASpace(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Chdir(t.TempDir())
	captureInitOutput(t, "--template", "agent", "sp ace3_agent")
	first, readable := goModOf(t, "sp ace3_agent")
	if first != "module github.com/example/my-agent" || !readable {
		t.Errorf("agent's go.mod starts %q (readable %v)", first, readable)
	}
}

func TestBasicModulePathIsTheImportPathCheckAndNotTheModulePathCheck(t *testing.T) {
	// "foo" has no dot in its first element, which module.CheckPath refuses and
	// `go mod init` and `go mod tidy` accept; refusing it would refuse nearly
	// every project name.
	for name, ok := range map[string]bool{"foo": true, "my_app": true, "app.v2": true, "a b": false, "a'b": false, "-x": false} {
		_, err := basicModulePath(filepath.Join(t.TempDir(), name))
		if (err == nil) != ok {
			t.Errorf("basicModulePath(%q): err %v, want ok=%v", name, err, ok)
		}
	}
}
