//go:build cgo

package csharp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// The determinism probe's measurements (cleat#4080, PR 6). packages/cleat-csharp/probe is a workflow component whose entries DELIBERATELY
// use the APIs cmd/cleat/vet_csharp.go has a rule about, and report what each did. This file builds it, runs each entry three ways, and
// prints the table the vet's rules cite:
//
//	fresh A     the entry run fresh
//	fresh B     the entry run fresh again, a moment later: shows the API VARIES at all (the control; a value that never varies cannot
//	            be shown to replay)
//	replay of A the entry replayed over A's recorded history: a field that differs from A is an API the replay does not reproduce
//
// A field is DETERMINISTIC when A == replay AND A != B somewhere would have been possible (the control column), NON-REPLAYED when replay
// differs from A, and THREW when the guest got an exception. The vet refuses by what was measured here, not by what the API's name
// suggests.

var (
	probeOnce sync.Once
	probeWasm []byte
	probeErr  string
)

func probeComponent(t *testing.T) []byte {
	t.Helper()
	needDocker(t)
	root := repoRoot(t)
	needPlatform(t, root, "linux/"+runtime.GOARCH)
	probeOnce.Do(func() {
		work, err := os.MkdirTemp("", "cleat-probe-")
		if err != nil {
			probeErr = fmt.Sprintf("no temporary directory: %v", err)
			return
		}
		defer os.RemoveAll(work)
		// build.sh directly, NOT `cleat build --target csharp`: the probe deliberately uses every API the determinism vet refuses, so the
		// build's gate would refuse it (TestCSharpVetCorpus holds that). The stamp is what the CLI adds after build.sh, and a component
		// without one is routed as Python, so it is written here the same way build_csharp.go writes it.
		stage := filepath.Join(work, "out")
		build := exec.Command("bash", filepath.Join(root, "packages", "cleat-csharp", "build.sh"), root, "packages/cleat-csharp/probe", stage)
		if log, err := build.CombinedOutput(); err != nil {
			probeErr = fmt.Sprintf("build.sh failed (exit 1 = the project was refused, 2 = UNMEASURED):\n%s", log)
			return
		}
		raw, err := os.ReadFile(filepath.Join(stage, "probe.wasm"))
		if err != nil {
			probeErr = fmt.Sprintf("no probe.wasm: %v", err)
			return
		}
		eps, err := os.ReadFile(filepath.Join(stage, "entry-points.txt"))
		if err != nil {
			probeErr = fmt.Sprintf("no entry-points.txt: %v", err)
			return
		}
		b, err := wasm.WriteMetadata(raw, &wasm.Metadata{
			WorkflowName: "probe", WorkflowVersion: 1, ABIVersion: wasm.CurrentABIVersion, MinCompatibleVersion: wasm.CurrentABIVersion,
			Language: "csharp", EntryPoints: strings.Fields(string(eps)),
		})
		if err != nil {
			probeErr = fmt.Sprintf("could not stamp the probe: %v", err)
			return
		}
		probeWasm = b
	})
	if probeErr != "" {
		t.Fatalf("the probe component was not built, so nothing was measured:\n%s", probeErr)
	}
	return probeWasm
}

type probeRun struct {
	result  map[string]string
	history []engine.EventRecord
	err     error
	calls   int
	susp    bool
}

func runProbe(t *testing.T, b []byte, entry, wfID string, startMs int64, replay []engine.EventRecord) probeRun {
	t.Helper()
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)
	caller := &recordingCaller{responses: map[string]string{
		"probe/first": `{"n":1}`, "probe/second": `{"n":2}`, "probe/third": `{"n":3}`,
	}}
	eng := engine.NewEngine(rt, caller, engine.WithBackends(engine.WasmtimeLanguages, wt), engine.WithWorkflowID(wfID),
		engine.WithWorkflowStartTime(startMs), engine.WithClock(func() int64 { return startMs }))
	var res string
	var hist []engine.EventRecord
	var susp *engine.SuspendResult
	if replay != nil {
		res, hist, susp, _, _, err = eng.Replay(ctx, b, entry, json.RawMessage(`{}`), replay)
	} else {
		res, hist, susp, _, _, err = eng.Execute(ctx, b, entry, json.RawMessage(`{}`))
	}
	out := probeRun{history: hist, err: err, calls: len(caller.recorded()), susp: susp != nil}
	if err == nil {
		m := map[string]string{}
		_ = json.Unmarshal([]byte(res), &m)
		out.result = m
	}
	return out
}

func TestCSharpDeterminismProbeMeasurements(t *testing.T) {
	b := probeComponent(t)
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: detected as %q", got)
	}
	meta, err := wasm.ReadMetadata(b)
	if err != nil {
		t.Fatal(err)
	}
	entries := append([]string(nil), meta.EntryPoints...)
	sort.Strings(entries)
	var report strings.Builder
	for _, entry := range entries {
		wf := "wf-probe-" + entry
		const start = int64(1_700_000_000_000)
		a := runProbe(t, b, entry, wf, start, nil)
		time.Sleep(1100 * time.Millisecond)
		bb := runProbe(t, b, entry, wf, start, nil)
		// c is the CONTROL: another workflow identity and another start time. A value the engine pins to the identity must differ here, or
		// "A equals the replay" would also be true of a constant the guest never read from anywhere.
		c := runProbe(t, b, entry, wf+"-other", start+86_400_000, nil)
		var r probeRun
		if a.err == nil {
			r = runProbe(t, b, entry, wf, start, a.history)
		}
		fmt.Fprintf(&report, "\n== %s  (A err=%v, B err=%v, replay err=%v, history=%d events, engine saw a suspension: %v, service calls received: %d)\n", entry, a.err, bb.err, r.err, len(a.history), a.susp, a.calls)
		keys := map[string]bool{}
		for k := range a.result {
			keys[k] = true
		}
		for k := range bb.result {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			var verdict string
			switch {
			case strings.HasPrefix(a.result[k], "THREW"):
				verdict = "THREW"
			case a.result[k] != r.result[k]:
				verdict = "NOT REPLAYED"
			case a.result[k] != c.result[k]:
				verdict = "REPLAYS, and follows the identity (A=R, A!=C)"
			case a.result[k] != bb.result[k]:
				verdict = "REPLAYS (A=R, varies between runs)"
			default:
				verdict = "constant (A=B=C=R: not shown to come from anywhere)"
			}
			fmt.Fprintf(&report, "  %-34s %-50s | A=%q B=%q C=%q R=%q\n", k, verdict, trunc(a.result[k]), trunc(bb.result[k]), trunc(c.result[k]), trunc(r.result[k]))
		}
	}
	t.Log(report.String())
	if os.Getenv("CLEAT_PROBE_REPORT") != "" {
		_ = os.WriteFile(os.Getenv("CLEAT_PROBE_REPORT"), []byte(report.String()), 0o644)
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
