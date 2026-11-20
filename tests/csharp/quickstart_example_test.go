//go:build cgo

package csharp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The C# quick start (docs/tutorials/csharp-quick-start.md) walks through examples/csharp-workflow. This builds that very project with
// `cleat build --target csharp` and runs it on the real engine and wasmtime backend, so the page describes a workflow that works rather than one
// that once did: the first run suspends in the sleep after one durable call, the resumed run answers that call from history and does not make it
// again, and the workflow's defer runs once when it finishes.

var (
	exampleOnce sync.Once
	exampleWasm []byte
	exampleErr  string
)

func exampleComponent(t *testing.T) []byte {
	t.Helper()
	needDocker(t)
	root := repoRoot(t)
	needPlatform(t, root, "linux/"+runtime.GOARCH)
	exampleOnce.Do(func() {
		work, err := os.MkdirTemp("", "cleat-csharp-example-")
		if err != nil {
			exampleErr = fmt.Sprintf("no temporary directory: %v", err)
			return
		}
		defer os.RemoveAll(work)
		bin := filepath.Join(work, "cleat")
		build := exec.Command("go", "build", "-o", bin, "./cmd/cleat")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			exampleErr = fmt.Sprintf("building cleat: %v\n%s", err, out)
			return
		}
		outDir := filepath.Join(work, "out")
		cmd := exec.Command(bin, "build", "--target", "csharp", "-o", outDir, "--version", "1", "examples/csharp-workflow")
		cmd.Dir = root
		if log, err := cmd.CombinedOutput(); err != nil {
			exampleErr = fmt.Sprintf("cleat build --target csharp examples/csharp-workflow failed (1 = a finding about the project, 2 = UNMEASURED): %v\n%s", err, log)
			return
		}
		b, err := os.ReadFile(filepath.Join(outDir, "orders.wasm"))
		if err != nil {
			exampleErr = fmt.Sprintf("the build succeeded but left no orders.wasm: %v", err)
			return
		}
		exampleWasm = b
	})
	if exampleErr != "" {
		t.Fatalf("the quick-start example was not built, so nothing below was measured:\n%s", exampleErr)
	}
	return exampleWasm
}

func TestCSharpQuickStartExampleSuspendsResumesFromHistoryAndRunsItsDefer(t *testing.T) {
	wasm := exampleComponent(t)
	responses := map[string]string{
		"payments/Charge": `{"id":"ch_1","amount":4200}`,
		"orders/Record":   `{"order":"o_1"}`,
	}
	input := `{"customer":"c-1","amount":4200}`

	first := runComponentEntry(t, wasm, "PlaceOrder", entryRun{WorkflowID: "wf-quickstart", Input: input, Responses: responses})
	if first.Err != nil {
		t.Fatalf("the first run failed: %v", first.Err)
	}
	if first.Suspended == nil {
		t.Fatalf("the first run did not suspend in the sleep; it finished with %q", first.Result)
	}
	if got := opsOf(first); strings.Join(got, ",") != "payments/Charge" {
		t.Fatalf("the first run made calls %v, want exactly payments/Charge before the sleep", got)
	}

	// Make the recorded charge look older than the sleep, so the resumed run finds the five seconds served. Exactly one such record, or the
	// helper fails: moving none would leave a resume that never wakes, and this test would pass for no reason.
	hist := moveRecordsBack(t, first.History, engine.EventTypeCall, 6_000)
	second := runComponentEntry(t, wasm, "PlaceOrder", entryRun{WorkflowID: "wf-quickstart", Input: input, Responses: responses, Replay: hist})
	if second.Err != nil || second.Suspended != nil {
		t.Fatalf("the resumed run did not finish: err=%v suspended=%v", second.Err, second.Suspended)
	}
	// Charge answers from history; Record is new; the defer's call is the last thing the run does.
	if got := strings.Join(opsOf(second), ","); got != "orders/Record,inventory/ReleaseHold" {
		t.Fatalf("the resumed run made calls %q, want orders/Record then inventory/ReleaseHold and NOT payments/Charge again", got)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal([]byte(second.Result), &doc); err != nil {
		t.Fatalf("the result is not the object the page shows: %v\n%s", err, second.Result)
	}
	if doc["charge"]["id"] != "ch_1" || doc["order"]["order"] != "o_1" {
		t.Fatalf("the result is %v, want the charge and the order the service answered", doc)
	}
}
