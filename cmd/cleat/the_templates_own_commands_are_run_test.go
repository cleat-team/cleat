package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#1947. TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds says in its
// own comment "only running the documented command does. So this runs the
// documented command." It does not. It runs `cleat build -o ./out .`, which is
// correct, while the templates documented something else:
//
//	workflow/Makefile:4   cleat build -o workflow.wasm .
//	agent/README.md:40    cleat build -o ./out/agent.wasm .
//
// `-o` is an output DIRECTORY (cmd/cleat/main.go:125). Passing a filename to it
// EXITS 0 and creates a directory named workflow.wasm containing process.wasm,
// so `make build` reports success and leaves nothing at the path `make deploy`
// then reads. Measured, not inferred.
//
// Three more, all found by running them rather than by reading:
//
//	workflow/Makefile:10   cleat deploy --db ...        exit 2, "flag provided but not defined: -db"
//	agent/README.md:43     cleat deploy --db ...        exit 2, same
//	fullstack/Makefile:19  cleat deploy my-fullstack-app ./out/my-fullstack-app.wasm
//	                                                    exit 1, "Error reading WASM file my-fullstack-app"
//
// --db is a GLOBAL flag (main.go:46) and must precede the subcommand; deploy
// takes the wasm path as its only operand, so a name written positionally is
// read as the path. And fullstack's documented path was wrong a second way:
// the artifact is named for the ENTRY POINT (submit_order.wasm), not the
// project.
//
// Five of the six documented invocations were broken. The one that worked,
// fullstack/Makefile:4, is the one the old test happened to hardcode.
//
// SO THIS TEST EXTRACTS THE COMMAND FROM THE SCAFFOLD AND RUNS THAT. A test
// that retypes the command it means to check can only ever confirm the author's
// understanding; it cannot see the file drift away from it.
func TestEveryTemplateDocumentedCommandActuallyRuns(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	// agent-python is absent for the same reason it is absent from the build
	// test: its documented first step is pip install, so running it would
	// measure the environment.
	for _, template := range []string{"workflow", "fullstack", "agent"} {
		t.Run(template, func(t *testing.T) {
			root := t.TempDir()
			name := "p_" + template
			if out, err := runCleatIn(t, root, "init", "--template", template, name); err != nil {
				t.Fatalf("cleat init --template %s: %v\n%s", template, err, out)
			}
			proj := filepath.Join(root, name)

			cmds := documentedCleatCommands(t, proj)
			// A scaffold whose documentation contains no runnable command
			// would make every assertion below vacuous, and the extractor
			// returning nothing looks exactly like a template with nothing to
			// check.
			if len(cmds) < 2 {
				t.Fatalf("extracted %d cleat commands from %s's own files; expected at least a "+
					"build and a deploy. Either the template stopped documenting them or the "+
					"extractor stopped seeing them -- and the second reads as success.", len(cmds), template)
			}

			var built, deployed int
			for _, c := range cmds {
				switch c.subcommand {
				case "build":
					out, err := runCleatIn(t, proj, c.args...)
					if err != nil {
						t.Errorf("%s documents `cleat %s` and it fails:\n%v\n%s",
							c.where, strings.Join(c.args, " "), err, out)
						continue
					}
					built++
				case "deploy":
					assertDeployArgsAreAccepted(t, proj, c)
					deployed++
				}
			}
			if built == 0 || deployed == 0 {
				t.Errorf("ran %d build and %d deploy commands from %s; both must be exercised",
					built, deployed, template)
			}
		})
	}
}

// assertDeployArgsAreAccepted runs a documented deploy command against an
// unreachable database and requires the failure to be about the DATABASE, not
// about the arguments.
//
// No database is needed to catch what cleat#1947 was about. Every defect there
// was rejected before a connection was attempted -- an undefined flag, or a
// wasm path that does not exist. Reaching "Error pinging database" means the
// flags parsed and the artifact was found and read, which is the whole claim.
func assertDeployArgsAreAccepted(t *testing.T, proj string, c documentedCommand) {
	t.Helper()
	cmd := exec.Command(cleatBinary, c.args...)
	cmd.Dir = proj
	cmd.Env = append(os.Environ(),
		"CLEAT_DATABASE_URL=postgres://u:p@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	raw, _ := cmd.CombinedOutput()
	out := string(raw)

	// Each of these is a real failure observed on develop, quoted from the run.
	for _, argFailure := range []struct{ needle, was string }{
		{"flag provided but not defined", "a global flag written after the subcommand (cleat#1933's shape)"},
		{"Error reading WASM file", "an operand that is not the artifact -- a name written positionally, or a stale path"},
		{"Usage: cleat deploy", "the operand was missing entirely"},
	} {
		if strings.Contains(out, argFailure.needle) {
			t.Errorf("%s documents `cleat %s`, which is rejected before any database is contacted: %s\n%s",
				c.where, strings.Join(c.args, " "), argFailure.was, out)
			return
		}
	}
}

type documentedCommand struct {
	where      string
	subcommand string
	args       []string
}

// documentedCleatCommands returns every `cleat ...` invocation a scaffolded
// project tells its user to run: Makefile recipe lines and fenced shell blocks
// in README.md.
//
// Read from the SCAFFOLD rather than from cmd/cleat/templates/, so that
// {{.ProjectName}} and friends are already substituted -- a command extracted
// from the template source would carry placeholders and could not be run.
func documentedCleatCommands(t *testing.T, proj string) []documentedCommand {
	t.Helper()
	var cmds []documentedCommand

	for _, file := range []string{"Makefile", "README.md"} {
		path := filepath.Join(proj, file)
		body, err := os.ReadFile(path)
		if err != nil {
			continue // not every template ships both
		}
		inFence := false
		lines := strings.Split(string(body), "\n")
		// A Makefile's own defaults (`export NAME ?= value`, `NAME = value`) are what `make` would substitute for
		// $(NAME) when the reader has set nothing, which is the case this test runs. Expanded here so a template
		// can parameterize a port without dropping its deploy line out of this test.
		vars := makeVariableDefaults(string(body))
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			if file == "README.md" {
				if strings.HasPrefix(strings.TrimSpace(line), "```") {
					inFence = !inFence
					continue
				}
				if !inFence {
					continue
				}
			} else if !strings.HasPrefix(line, "\t") {
				continue // only Makefile RECIPE lines are commands
			}

			// Join shell line-continuations, which is how the agent README
			// writes its deploy command.
			joined := strings.TrimSpace(line)
			for strings.HasSuffix(joined, `\`) && i+1 < len(lines) {
				i++
				joined = strings.TrimSuffix(joined, `\`) + " " + strings.TrimSpace(lines[i])
			}

			for name, value := range vars {
				joined = strings.ReplaceAll(joined, "$("+name+")", value)
			}
			fields := strings.Fields(joined)
			if len(fields) < 2 || fields[0] != "cleat" {
				continue
			}
			// A command carrying an unexpanded Make variable cannot be run
			// here. Reported rather than skipped silently: a template that
			// moved its whole deploy line behind $(VAR) would otherwise drop
			// out of this test without a word.
			if strings.Contains(joined, "$(") {
				t.Errorf("%s documents `%s`, which this test cannot run because it depends on a "+
					"Make variable. It is therefore unchecked; either inline the value or extend "+
					"this extractor.", path, joined)
				continue
			}

			args := fields[1:]
			sub := ""
			for _, a := range args {
				// Skip GLOBAL flags and their values to find the subcommand.
				// `cleat --db X deploy ...` is the correct spelling; treating
				// args[0] as the subcommand is what made #1933's defect look
				// like a missing flag.
				if strings.HasPrefix(a, "-") {
					continue
				}
				if sub == "" && (a == "build" || a == "deploy" || a == "run" || a == "init" || a == "dev") {
					sub = a
					break
				}
			}
			if sub == "" {
				continue
			}
			cmds = append(cmds, documentedCommand{
				where:      path + " (" + file + ")",
				subcommand: sub,
				args:       args,
			})
		}
	}
	return cmds
}

// makeVariableDefaults reads the simple variable assignments a Makefile makes at the top level: NAME = v,
// NAME := v, NAME ?= v, each optionally prefixed with `export`. It does not evaluate anything, so a value that
// itself contains $( is left for the caller to report as unexpanded.
func makeVariableDefaults(makefile string) map[string]string {
	vars := map[string]string{}
	assign := regexp.MustCompile(`^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?:\?=|:=|=)\s*(.*?)\s*$`)
	for _, line := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := assign.FindStringSubmatch(line); m != nil {
			vars[m[1]] = m[2]
		}
	}
	return vars
}
