// quickStart holds the pieces of README.md's "## Quick Start" fenced block that
// TestREADMEQuickStartReachesADoneWorkflow needs to actually run steps 3/4/7 --
// extracted from the file, not retyped as Go string literals.
//
// FALSIFIED (cleat-review R2, cleat#2798): before this extraction existed, the test hardcoded
// "./testdata/hello/", "greet.wasm", "hello" and the curl body directly, so it kept passing
// even if README's step 3 were reverted to "./testdata/basic/" -- the exact regression #2788
// exists to catch, reproduced by its own guard. Falsified by doing that revert: with
// buildTarget hardcoded, the test still built testdata/hello regardless of what README said and
// stayed green. With extraction in place, the same revert makes parseQuickStart return
// "./testdata/basic/", the test builds and deploys THAT, and it fails for the real reason --
// PlaceOrder's unconfigured "catalog" DurableCall -- not a string comparison. Restored
// afterward; `git diff --stat` was empty before re-running to confirm the revert left no trace.
package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

type quickStart struct {
	buildTarget  string // the package path `cleat build` compiles, e.g. "./testdata/hello/"
	wasmBasename string // the .wasm file `cleat deploy` names, e.g. "greet.wasm"
	deployName   string // the --name `cleat deploy` uses, e.g. "hello"
	startName    string // the workflow name in the curl path, e.g. "hello"
	startBody    string // the curl -d body, e.g. `{"input":"Ada"}`
}

var (
	qsBuildRe    = regexp.MustCompile(`cleat build\s+-o\s+\S+\s+(\S+)`)
	qsDeployRe   = regexp.MustCompile(`cleat deploy\b[^\n]*?--name\s+(\S+)[^\n]*?([^\s"'` + "`" + `]+\.wasm)`)
	qsCurlPathRe = regexp.MustCompile(`/api/workflows/([^/\s]+)/start`)
	qsCurlBodyRe = regexp.MustCompile(`-d\s+'([^']*)'`)
)

// parseQuickStart extracts the pieces of README.md's Quick Start block that
// TestREADMEQuickStartReachesADoneWorkflow needs to run steps 3/4/7 for real, so a change to
// README's build target, deploy name/artifact, or trigger route/body changes what the test runs
// rather than only what it would have to be told to expect.
func parseQuickStart(t *testing.T, readmePath string) quickStart {
	t.Helper()
	data, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}
	block := bashBlockAfterHeading(t, string(data), "## Quick Start")
	joined := joinLineContinuations(strings.Join(block, "\n"))

	buildM := qsBuildRe.FindStringSubmatch(joined)
	if buildM == nil {
		t.Fatalf("Quick Start block has no `cleat build -o <dir> <target>` line -- README or "+
			"extractor drifted. Block:\n%s", joined)
	}

	deployM := qsDeployRe.FindStringSubmatch(joined)
	if deployM == nil {
		t.Fatalf("Quick Start block has no `cleat deploy ... --name <name> <wasm>` line -- README "+
			"or extractor drifted. Block:\n%s", joined)
	}

	pathM := qsCurlPathRe.FindStringSubmatch(joined)
	if pathM == nil {
		t.Fatalf("Quick Start block has no `curl .../api/workflows/<name>/start` line -- README "+
			"or extractor drifted. Block:\n%s", joined)
	}

	bodyM := qsCurlBodyRe.FindStringSubmatch(joined)
	if bodyM == nil {
		t.Fatalf("Quick Start block's curl line has no `-d '<body>'` -- README or extractor "+
			"drifted. Block:\n%s", joined)
	}

	return quickStart{
		buildTarget:  buildM[1],
		wasmBasename: pathBase(deployM[2]),
		deployName:   deployM[1],
		startName:    pathM[1],
		startBody:    bodyM[1],
	}
}

// pathBase is filepath.Base without importing path/filepath twice in this file's tiny surface --
// README's documented wasm path is always forward-slashed (it is a Unix shell example), so a
// plain split is exact and does not need the OS-aware package.
func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// bashBlockAfterHeading returns the lines inside the first ```bash fenced block that appears
// AFTER a given markdown heading line, so a caller can target a specific section's block rather
// than always the file's first one (see firstBashBlock in readme_first_command_completes_test.go
// for that narrower case, which this does not replace -- README.md's "try it" snippet has no
// heading of its own to anchor on, sitting above every `##` heading in the file).
func bashBlockAfterHeading(t *testing.T, markdown, heading string) []string {
	t.Helper()
	lines := strings.Split(markdown, "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == heading {
			break
		}
	}
	if i == len(lines) {
		t.Fatalf("no line matches heading %q", heading)
	}
	inFence := false
	var block []string
	for ; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !inFence {
			if trimmed == "```bash" {
				inFence = true
			}
			continue
		}
		if trimmed == "```" {
			return block
		}
		block = append(block, lines[i])
	}
	t.Fatalf("heading %q has no fenced ```bash block after it, or it is never closed", heading)
	return nil
}
