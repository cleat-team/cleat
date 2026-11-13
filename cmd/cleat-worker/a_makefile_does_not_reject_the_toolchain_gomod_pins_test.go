package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cleat#3787: `make tools-go` (and so `make setup`) refuses Go at or above
// GO_CGO_BROKEN_FROM, because that Go cannot link wasmtime. go.mod's `toolchain`
// line is the Go this repository actually builds with.
//
// If the two ever cross, `make setup` rejects the toolchain the repository pins:
// the check turns from a warning about someone else's Go into a refusal of ours.
// That is what happens when the hold on Go 1.27.4 (D78, cleat#3701) is lifted by
// moving go.mod forward and nobody remembers the Makefile. This fails at that
// moment, in the PR that bumps go.mod, which is the reminder.

func goVersionParts(t *testing.T, v string) [3]int {
	t.Helper()
	m := regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`).FindStringSubmatch(strings.TrimPrefix(v, "go"))
	if m == nil {
		t.Fatalf("not a Go version: %q", v)
	}
	var out [3]int
	for i := 1; i <= 3; i++ {
		if m[i] != "" {
			n, err := strconv.Atoi(m[i])
			if err != nil {
				t.Fatal(err)
			}
			out[i-1] = n
		}
	}
	return out
}

func goVersionAtLeast(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func TestGoVersionComparisonOrdersTheWayTheMakefileDoes(t *testing.T) {
	// The comparison below is only worth trusting if it can say both answers.
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.27.3", "1.27.4", false},
		{"1.27.4", "1.27.4", true},
		{"1.27.10", "1.27.4", true}, // 10 > 4: a string compare gets this wrong
		{"1.28.0", "1.27.4", true},
		{"1.27", "1.27.4", false},
		{"1.26.9", "1.27.0", false},
	} {
		if got := goVersionAtLeast(goVersionParts(t, c.a), goVersionParts(t, c.b)); got != c.want {
			t.Errorf("%s >= %s = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTheMakefileDoesNotRejectTheToolchainGoModPins(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	gm, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	bm := regexp.MustCompile(`(?m)^GO_CGO_BROKEN_FROM\s*:=\s*(\S+)\s*$`).FindSubmatch(mk)
	if bm == nil {
		t.Fatal("Makefile has no `GO_CGO_BROKEN_FROM :=` line; the check in tools-go reads it, and this test " +
			"is what ties it to go.mod. If the hold was lifted and the variable removed on purpose, delete this test with it.")
	}
	tm := regexp.MustCompile(`(?m)^toolchain\s+(go\S+)\s*$`).FindSubmatch(gm)
	if tm == nil {
		t.Fatal("go.mod has no `toolchain` line")
	}
	broken, pinned := goVersionParts(t, string(bm[1])), goVersionParts(t, string(tm[1]))
	if goVersionAtLeast(pinned, broken) {
		t.Errorf("go.mod pins toolchain %s, which is at or above the Makefile's GO_CGO_BROKEN_FROM (%s): "+
			"`make setup` now rejects the Go this repository builds with.\n"+
			"If the Go 1.27.4 hold (D78, cleat#3701) was lifted, update or delete GO_CGO_BROKEN_FROM and the "+
			"[BROKEN] branch of tools-go, then this test.", tm[1], bm[1])
	}
}

// TestMakeSetupRefusesAGoThatCannotLinkWasmtime runs the Makefile's own
// tools-go against a stand-in `go`, because the threshold above is only half of
// it: the other half is that the branch really stops. `make setup` used to print
// "[OK] Go go1.27.4" and let the next step fail with a linker message; every
// other branch of tools-go (including [OLD] and [MISSING]) exits 0, so "it
// printed something" proves nothing and the exit status is what is asserted.
func TestMakeSetupRefusesAGoThatCannotLinkWasmtime(t *testing.T) {
	// Not a skip: make is the tool the quick-start tells every reader to run, so a
	// machine running this repository's tests without it is a broken machine, and
	// a skip here would be a green that checked nothing.
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatalf("make is not on PATH: %v", err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	run := func(goVersion string) (int, string) {
		dir := t.TempDir()
		stub := "#!/bin/sh\necho \"go version " + goVersion + " linux/amd64\"\n"
		if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("make", "-C", root, "tools-go")
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err == nil {
			return 0, out.String()
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out.String()
		}
		t.Fatal(err)
		return -1, ""
	}

	// The known-positive: the Go before the break is accepted, with no complaint.
	if code, out := run("go1.27.3"); code != 0 || !strings.Contains(out, "[OK]") || strings.Contains(out, "BROKEN") {
		t.Errorf("go1.27.3: exit %d, want 0 and [OK]:\n%s", code, out)
	}
	for _, v := range []string{"go1.27.4", "go1.27.10", "go1.28.0"} {
		code, out := run(v)
		if code == 0 {
			t.Errorf("%s: make tools-go exited 0; it must stop.\n%s", v, out)
		}
		for _, want := range []string{"[BROKEN]", "GOTOOLCHAIN=go1.27.3", "releases"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: the refusal does not contain %q:\n%s", v, want, out)
			}
		}
		if strings.Contains(out, "[OK]") {
			t.Errorf("%s: printed [OK] for a Go that cannot link cleat:\n%s", v, out)
		}
	}
}
