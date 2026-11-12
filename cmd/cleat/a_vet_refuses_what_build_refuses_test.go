package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3524. `cleat build` took two refusals that `cleat vet` never called: a
// //cleat:exposure that names no class or two classes (cleat#1986), and
// `--channel frozen` over a child whose name is not a constant (cleat#3500).
// Measured before the change on testdata/exposureconflict, testdata/exposureinvalid
// and testdata/childnames: build exit 1, vet exit 0 and printing OK, vet having no
// --channel flag at all. The exposure one also fired only after the compile, so
// the refused build left a .wasm behind.
//
// The table is the claim "one answer": every row runs the five forms and asserts
// one verdict and one message across them. The accepted rows are the floor: a
// gate that refused everything would pass the refused rows.
func TestVetAndBuildGiveOneAnswerOnTheRefusalsOnlyBuildUsedToTake(t *testing.T) {
	needBinary(t)
	td := func(name string) string { return filepath.Join("..", "..", "testdata", name) }

	for _, c := range []struct {
		name    string
		pkg     string
		channel string // "" = flag not passed
		refuse  string // "" = must be accepted; otherwise a text every form must carry
	}{
		{"conflicting exposure classes", td("exposureconflict"), "", "declares conflicting classes (internal, public)"},
		{"an exposure class that is not one", td("exposureinvalid"), "", `got "bogus"`},
		{"frozen over an unpinnable child", td("childnames"), "frozen", "--channel frozen cannot pin 2 child workflow start(s)"},
		{"stable over the same child", td("childnames"), "stable", ""},
		{"one valid exposure class", td("exposure"), "", ""},
		{"a plain package", td("basic"), "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Flags go BEFORE the path: the CLI refuses one after it, which is
			// right, and which a test that appended it would trip over.
			args := func(sub string, rest ...string) []string {
				a := []string{sub}
				if c.channel != "" {
					a = append(a, "--channel", c.channel)
				}
				return append(append(a, rest...), c.pkg)
			}
			outPlain := filepath.Join(t.TempDir(), "plain")
			outJSON := filepath.Join(t.TempDir(), "json")

			vetOut, vetCode := runCleat(t, args("vet")...)
			vetJSON, vetJSONCode := runCleat(t, args("vet", "--json")...)
			vetCI, vetCICode := runCleat(t, args("vet", "--ci")...)
			buildOut, buildCode := runCleat(t, args("build", "-o", outPlain)...)
			buildJSON, buildJSONCode := runCleat(t, args("build", "--json", "-o", outJSON)...)

			want := 0
			if c.refuse != "" {
				want = 1
			}
			for form, code := range map[string]int{
				"vet": vetCode, "vet --json": vetJSONCode, "vet --ci": vetCICode,
				"build": buildCode, "build --json": buildJSONCode,
			} {
				if code != want {
					t.Errorf("`cleat %s` exited %d, want %d (the five forms must agree)", form, code, want)
				}
			}
			if c.refuse == "" {
				if strings.Contains(vetCI, "::error") {
					t.Errorf("`cleat vet --ci` annotated an error on a package every other form accepts:\n%s", vetCI)
				}
				return
			}

			// --ci has its own loop and its own exit status, so it is a form of
			// its own. Its annotation is one line (newlines travel as %0A), and
			// every refusal text asserted here is on the first line.
			if !strings.Contains(vetCI, "::error") {
				t.Errorf("`cleat vet --ci` printed no error annotation for a refusal:\n%s", vetCI)
			}
			for form, out := range map[string]string{"vet": vetOut, "build": buildOut, "vet --ci": vetCI} {
				if !strings.Contains(out, c.refuse) {
					t.Errorf("`cleat %s` refused without saying %q:\n%s", form, c.refuse, out)
				}
			}
			// Same message, not merely the same status: the JSON diagnostics of
			// the two commands must carry the identical error list.
			vm, bm := jsonErrorMessages(t, vetJSON), jsonErrorMessages(t, buildJSON)
			if strings.Join(vm, "\n") != strings.Join(bm, "\n") {
				t.Errorf("vet --json and build --json report different errors:\n  vet:   %q\n  build: %q", vm, bm)
			}
			found := false
			for _, m := range vm {
				found = found || strings.Contains(m, c.refuse)
			}
			if !found {
				t.Errorf("the refusal is not in the JSON errors: %q", vm)
			}

			// A refused build must leave nothing behind. The exposure refusal
			// used to run after the compile and left order.wasm without its
			// metadata, which clew's CI had to work around.
			for _, dir := range []string{outPlain, outJSON} {
				entries, err := os.ReadDir(dir)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					names := make([]string, 0, len(entries))
					for _, e := range entries {
						names = append(names, e.Name())
					}
					t.Errorf("a refused build left files in %s: %v", dir, names)
				}
			}
		})
	}
}

// runCleat runs the binary and returns stdout+stderr and the exit code. A
// process that could not be started is a test failure, not a code, so a missing
// binary cannot read as "refused".
func runCleat(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(cleatBinary, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), ee.ExitCode()
	}
	t.Fatalf("could not run cleat %v: %v", args, err)
	return "", -1
}

// jsonErrorMessages returns the error messages of a --json run. The output is
// stdout and stderr together, so it takes the one line that is the JSON object.
func jsonErrorMessages(t *testing.T, out string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var vo VetOutput
		if err := json.Unmarshal([]byte(line), &vo); err != nil {
			continue
		}
		msgs := make([]string, 0, len(vo.Errors))
		for _, e := range vo.Errors {
			msgs = append(msgs, e.Message)
		}
		return msgs
	}
	t.Fatalf("no JSON diagnostics in output:\n%s", out)
	return nil
}
