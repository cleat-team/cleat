package wasm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAbsoluteEntryPath covers cleat#1836.
//
// BuildPythonWasmWithRuntime runs build_wasm.py with cmd.Dir set to the SDK
// root, and that script resolves its --entry with a bare Path(entry_file). So
// a RELATIVE entry was looked up under python-sdk/ rather than under the
// directory the user ran the command in, and both documented Python example
// commands failed:
//
//	$ cd examples/python-langchain
//	$ cleat build --target python --entry research_agent.py:langchain_research_agent
//	Error: Entry file not found: research_agent.py     <- it is right there
//
// # Why this is testable without a Python toolchain
//
// The whole behaviour is path arithmetic, so it is pulled out of the exec path
// and asserted directly. The alternative is a test that needs componentize-py
// installed to say anything, which on most machines means a test that says
// nothing -- and the failure it is guarding is precisely the kind that hides
// behind a missing toolchain. The end-to-end behaviour was checked separately
// with a stubbed componentize-py: before the fix the script reported "Entry
// file not found", after it "Validated entry: /…/research_agent.py".
func TestAbsoluteEntryPath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a relative path is resolved against the process directory", func(t *testing.T) {
		got := AbsoluteEntryPath("workflow.py:run")
		want := filepath.Join(cwd, "workflow.py") + ":run"
		if got != want {
			t.Errorf("got %q, want %q\n\nThis is the whole of cleat#1836: the script "+
				"runs with cmd.Dir at the SDK root, so a path left relative is looked "+
				"up in the wrong tree.", got, want)
		}
	})

	t.Run("a nested relative path keeps its shape", func(t *testing.T) {
		got := AbsoluteEntryPath("examples/python-hello/hello_workflow.py:hello")
		want := filepath.Join(cwd, "examples/python-hello/hello_workflow.py") + ":hello"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("an already-absolute path is unchanged", func(t *testing.T) {
		in := filepath.Join(cwd, "workflow.py") + ":run"
		if got := AbsoluteEntryPath(in); got != in {
			t.Errorf("got %q, want it unchanged: %q", got, in)
		}
	})

	t.Run("the function name is never touched", func(t *testing.T) {
		// Guards the split, not the resolution. An entry whose FUNCTION name
		// is the only thing after the last colon must keep it intact, or the
		// build silently looks for the wrong symbol.
		got := AbsoluteEntryPath("a/b.py:some_function_name")
		if !strings.HasSuffix(got, ":some_function_name") {
			t.Errorf("got %q, which lost or altered the function name", got)
		}
	})

	t.Run("the LAST colon splits, matching parse_entry", func(t *testing.T) {
		// build_wasm.py uses entry.rsplit(":", 1). A directory containing a
		// colon must resolve the same way on both sides, or Go and Python
		// disagree about which half is the path.
		//
		// THE CASE HAS TO CONTAIN SOMETHING Abs NORMALISES, and the first
		// version of this test did not. With "od:d/workflow.py:run", splitting
		// on the first colon gives Abs("od") + ":d/workflow.py:run" and
		// splitting on the last gives Abs("od:d/workflow.py") + ":run" -- and
		// those are the SAME STRING, because Abs of a clean relative path is
		// just cwd + "/" + path, so re-joining after any split reproduces it.
		// Measured: swapping LastIndex for Index failed nothing.
		//
		// The ".." below is what separates them. Resolving the whole path
		// collapses it; resolving only "od" leaves it in the function half,
		// where nothing will ever clean it.
		got := AbsoluteEntryPath("od:d/../e/workflow.py:run")
		// ".." removes the WHOLE preceding element "od:d", not just "d" --
		// measured rather than reasoned, after a first expectation of
		// "od:e/workflow.py" turned out to be wrong.
		want := filepath.Join(cwd, "e/workflow.py") + ":run"
		if got != want {
			t.Errorf("got %q, want %q\n\nSplitting on the FIRST colon sends \"od\" "+
				"as the path and leaves \"d/../e/workflow.py:run\" as the function "+
				"name, so the .. is never resolved.", got, want)
		}
	})

	// Entries with no file half are passed through for build_wasm.py's
	// parse_entry to reject with its own message. Reinterpreting them here
	// would replace a clear error with a confusing one.
	for _, in := range []string{"workflow.py", "", ":run"} {
		t.Run("passed through untouched: "+describe(in), func(t *testing.T) {
			if got := AbsoluteEntryPath(in); got != in {
				t.Errorf("got %q, want it unchanged: %q\n\nThere is no file half to "+
					"resolve; parse_entry should reject this, not this function.", got, in)
			}
		})
	}
}

func describe(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
