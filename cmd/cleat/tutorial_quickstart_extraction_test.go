// tutorialQuickStart holds the pieces of docs/tutorials/quick-start.md that
// TestTutorialQuickStartReachesADoneWorkflow needs to actually run the
// tutorial -- extracted from the file, not retyped as Go string literals.
// Same technique as cmd/cleat/quickstart_extraction_test.go's parseQuickStart
// (README.md's Quick Start), and the same reason: cleat-review's R2 on
// cleat#2798 found that a guard which hardcodes the fixture names it expects
// stays green even when the documented commands are reverted to a different,
// broken fixture, because nothing in the guard ever reads the doc. Extracting
// makes a change to the documented build target, scaffold name, deploy
// name/artifact, or trigger route/body change what this test RUNS, not just
// what it compares the result against.
//
// The one structural difference from parseQuickStart: README's Quick Start is
// one combined ```bash fenced block; this tutorial has one separate block per
// numbered step (11 steps). allBashBlocks collects every ```bash block in the
// whole file, in order, and joins them -- the five patterns below are each
// distinctive enough (a literal subcommand plus a flag no other step's block
// contains) that scanning the whole joined text is as safe as anchoring to one
// step's heading would be, and needs no per-heading bookkeeping.
package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

type tutorialQuickStart struct {
	scaffoldName string // `cleat init <name>`'s positional arg, e.g. "my-workflow"
	buildOutDir  string // `cleat build -o <dir> .`'s -o argument, e.g. "workflow.wasm"
	wasmBasename string // the deploy line's wasm path's basename, e.g. "hello.wasm"
	deployName   string // the deploy line's --name, e.g. "my-workflow"
	startName    string // the workflow name in the trigger curl's path
	startBody    string // the trigger curl's -d body
}

var (
	tqsInitRe       = regexp.MustCompile(`(?m)cleat init\s+(\S+)\s*$`)
	tqsBuildRe      = regexp.MustCompile(`(?m)cleat build\s+-o\s+(\S+)\s+\.\s*$`)
	tqsDeployRe     = regexp.MustCompile(`cleat deploy\b[^\n]*?--name\s+(\S+)[^\n]*?([^\s"'` + "`" + `]+\.wasm)`)
	tqsCurlStartRe  = regexp.MustCompile(`/api/workflows/([^/\s]+)/start`)
	tqsCurlBodyRe   = regexp.MustCompile(`-d\s+'([^']*)'`)
	tqsFenceOpenRe  = regexp.MustCompile("^```bash\\s*$")
	tqsFenceCloseRe = regexp.MustCompile("^```\\s*$")
)

// parseTutorialQuickStart extracts docs/tutorials/quick-start.md's fixture
// identifiers so TestTutorialQuickStartReachesADoneWorkflow runs the
// documented scaffold, build target, deploy name/artifact and trigger
// route/body -- not a copy retyped into this file.
func parseTutorialQuickStart(t *testing.T, path string) tutorialQuickStart {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	joined := joinLineContinuations(strings.Join(allBashBlocks(t, string(data)), "\n"))

	initM := tqsInitRe.FindStringSubmatch(joined)
	if initM == nil {
		t.Fatalf("tutorial has no `cleat init <name>` line -- the tutorial or the extractor drifted. Blocks:\n%s", joined)
	}
	buildM := tqsBuildRe.FindStringSubmatch(joined)
	if buildM == nil {
		t.Fatalf("tutorial has no `cleat build -o <dir> .` line -- the tutorial or the extractor drifted. Blocks:\n%s", joined)
	}
	deployM := tqsDeployRe.FindStringSubmatch(joined)
	if deployM == nil {
		t.Fatalf("tutorial has no `cleat deploy ... --name <name> <wasm>` line -- the tutorial or the extractor drifted. Blocks:\n%s", joined)
	}
	startM := tqsCurlStartRe.FindStringSubmatch(joined)
	if startM == nil {
		t.Fatalf("tutorial has no `curl .../api/workflows/<name>/start` line -- the tutorial or the extractor drifted. Blocks:\n%s", joined)
	}
	bodyM := tqsCurlBodyRe.FindStringSubmatch(joined)
	if bodyM == nil {
		t.Fatalf("tutorial's trigger curl has no `-d '<body>'` -- the tutorial or the extractor drifted. Blocks:\n%s", joined)
	}

	return tutorialQuickStart{
		scaffoldName: initM[1],
		buildOutDir:  buildM[1],
		wasmBasename: pathBase(deployM[2]),
		deployName:   deployM[1],
		startName:    startM[1],
		startBody:    bodyM[1],
	}
}

// allBashBlocks returns every ```bash fenced block's lines, in file order,
// each block terminated by an implicit blank separator -- a caller that joins
// them with "\n" (as parseTutorialQuickStart does) gets one block's last line
// adjacent to the next block's first, which matters only if a pattern could
// span two unrelated blocks; none of the patterns above do, since each is
// anchored to one complete documented command.
func allBashBlocks(t *testing.T, markdown string) []string {
	t.Helper()
	lines := strings.Split(markdown, "\n")
	var blocks []string
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case !inFence && tqsFenceOpenRe.MatchString(trimmed):
			inFence = true
		case inFence && tqsFenceCloseRe.MatchString(trimmed):
			inFence = false
		case inFence:
			blocks = append(blocks, line)
		}
	}
	if len(blocks) == 0 {
		t.Fatalf("no ```bash fenced block found in the tutorial -- the tutorial or the extractor drifted")
	}
	return blocks
}

// TestParseTutorialQuickStart is parseTutorialQuickStart's own known-positive:
// confirms it extracts exactly what docs/tutorials/quick-start.md documents
// today, so a future regression in the extractor itself (not the tutorial)
// is caught here rather than surfacing as a confusing failure deep inside
// TestTutorialQuickStartReachesADoneWorkflow.
func TestParseTutorialQuickStart(t *testing.T) {
	root := repoRoot(t)
	qs := parseTutorialQuickStart(t, root+"/docs/tutorials/quick-start.md")
	if qs.scaffoldName != "my-workflow" {
		t.Errorf("scaffoldName = %q, want %q", qs.scaffoldName, "my-workflow")
	}
	if qs.buildOutDir != "workflow.wasm" {
		t.Errorf("buildOutDir = %q, want %q", qs.buildOutDir, "workflow.wasm")
	}
	if qs.wasmBasename != "my-workflow.wasm" {
		t.Errorf("wasmBasename = %q, want %q", qs.wasmBasename, "my-workflow.wasm")
	}
	if qs.deployName != "my-workflow" {
		t.Errorf("deployName = %q, want %q", qs.deployName, "my-workflow")
	}
	if qs.startName != "my-workflow" {
		t.Errorf("startName = %q, want %q", qs.startName, "my-workflow")
	}
	if qs.startBody != `{"input": "World"}` {
		t.Errorf("startBody = %q, want %q", qs.startBody, `{"input": "World"}`)
	}
}
