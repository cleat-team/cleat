//go:build cgo

package engine

// cleat#4260. tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm is a deliberate fixture (IMPROVEMENT-PLAN 3.308: the committed bytes
// carry 9 occurrences of register_query_handler, a fresh build carries 1, and engine/imports.go cites them) that CANNOT INSTANTIATE on the current
// engine: it imports durable-send-signal-and-wait, which 3.220 removed. Three tests read it for properties that do not need it to run, and one of them
// (TestTheCheckedInPythonComponentIsCompiledOnce) says so in a comment. Nothing checked the claim, so it could rot in either direction: the fixture
// could be refreshed, or the imports could change, and every comment saying "stale" would be false and nothing would notice.
//
// This test makes the claim a checked fact. It asserts the specific failure, a LINK failure naming the removed import, and not merely "an error":
// a trap, a timeout or a missing export is a different problem and must not be read as "stale as documented".
//
// WHEN THIS TEST FAILS, the fixture's status changed. Update tests/plugin-harness/testdata/pythonworkflow/README.md, the comment on
// TestTheCheckedInPythonComponentIsCompiledOnce, and the witness note on RegisterQueryHandler in engine/imports.go, which cites this file as proof that
// removing an import breaks guests already compiled against it. That note is only true while the fixture is a guest that exercises that import.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTheCheckedInPythonComponentDoesNotLinkAndSaysWhy(t *testing.T) {
	wasm, err := os.ReadFile("../tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm")
	if err != nil {
		t.Fatalf("the checked-in Python component is missing: %v", err)
	}
	if !isComponentWasm(wasm) {
		t.Fatal("call_all_plugins.wasm is not a Component Model binary, so this would not reach the component path")
	}
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	_, err = b.PerExecution(0).Execute(context.Background(), wasm, "run", json.RawMessage(`{}`), &mockHostHandler{})
	if err == nil {
		t.Fatal("the checked-in Python component now instantiates and ran. It was documented as stale (cleat#4260): " +
			"it imports durable-send-signal-and-wait, which IMPROVEMENT-PLAN 3.220 removed. The fixture was refreshed or the import came back; " +
			"update the README beside the fixture, the comment on TestTheCheckedInPythonComponentIsCompiledOnce and the witness note in engine/imports.go")
	}
	msg := err.Error()
	for _, want := range []string{"not found in the linker", "durable-send-signal-and-wait"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the checked-in Python component failed, but not as documented (a link failure naming the removed import):\n  missing %q\n  error: %s", want, msg)
		}
	}
}
