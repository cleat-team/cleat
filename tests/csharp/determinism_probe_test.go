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

// probeWant is the measurement table, asserted. TestCSharpDeterminismProbeMeasurements fails when any row, or the SET of rows, differs.
// The vet's PERMITTED half (DateTime, Random, Guid, hash codes, Task.Yield, ThreadPool) rests on these rows being what they are: a toolchain or
// engine change that broke replay for one of them would otherwise leave the vet allowing it with every test green (cleat-review2, #4164).
// Verdicts are the first word(s) of the report column: THREW, NOT REPLAYED, REPLAYS (follows the identity), constant.
// A value may name alternatives with "|" where the measurement itself is not stable. Environment.TickCount64 is the one: it is a few milliseconds
// since the instance started, so a run and its replay read the same number whenever the two instance start-ups take the same time. Over nine
// recorded probe runs seven differed and two coincided, and spacing five readings 7 ms apart made no difference (the offset is what coincides).
// It is NOT pinned by the engine: the other clock in the same entry, Stopwatch.GetTimestamp (nanoseconds), differs on every run.
var probeWant = map[string]map[string]string{
	"probe_async": {
		"both":   "constant",
		"first":  "constant",
		"second": "constant",
		"third":  "constant",
	},
	"probe_clock": {
		"DateTime.Now":          "REPLAYS, and follows the identity",
		"DateTime.UtcNow":       "REPLAYS, and follows the identity",
		"DateTimeOffset.UtcNow": "REPLAYS, and follows the identity",
		"TimeZoneInfo.Local":    "constant",
		"h.Now":                 "REPLAYS, and follows the identity",
	},
	"probe_entropy": {
		"Guid.NewGuid":               "REPLAYS, and follows the identity",
		"Random.Shared":              "REPLAYS, and follows the identity",
		"RandomNumberGenerator":      "THREW",
		"RuntimeHelpers.GetHashCode": "REPLAYS, and follows the identity",
		"h.Random":                   "REPLAYS, and follows the identity",
		"new Random()":               "REPLAYS, and follows the identity",
		"new Random(42)":             "constant",
		"string.GetHashCode":         "REPLAYS, and follows the identity",
	},
	"probe_environment": {
		"CurrentDirectory":             "constant",
		"GetEnvironmentVariable(PATH)": "constant",
		"MachineName":                  "constant",
		"OSVersion":                    "constant",
		"ProcessId":                    "constant",
		"UserName":                     "constant",
	},
	"probe_io": {
		"Console.ReadLine":              "constant",
		"Console.WriteLine":             "constant",
		"Directory.GetCurrentDirectory": "constant",
		"File.Exists":                   "constant",
		"File.ReadAllText":              "THREW",
		"File.WriteAllText":             "THREW",
	},
	"probe_monotonic": {
		"Environment.TickCount64": "NOT REPLAYED|REPLAYS, and follows the identity", // coincides by chance: see the comment on probeWant
		"Stopwatch.GetTimestamp":  "NOT REPLAYED",
	},
	"probe_tasks": {
		"Task.Delay":                      "THREW",
		"Task.Factory.StartNew":           "THREW",
		"Task.Run":                        "THREW",
		"Task.Wait on an incomplete task": "THREW",
		"Task.Yield":                      "constant",
		"ThreadPool.QueueUserWorkItem":    "constant",
		"new Thread":                      "THREW",
	},
	"probe_thread_sleep": {
		"Thread.Sleep(50)": "constant",
		"elapsed-ms>=45":   "constant",
	},
}

// probeSwallowWant is the swallowing-catch half of the table: how many host calls the SERVICE received from each entry, which sleeps a minute
// inside the catch under test and then calls the host once AFTER it, and whether the engine saw the suspension. The first two let that call
// reach the service (1); the filtered and rethrown forms did not (0). CV101 rests on this.
var probeSwallowWant = map[string]struct {
	calls int
	susp  bool
}{
	"probe_swallow_bare":      {1, true},
	"probe_swallow_exception": {1, true},
	"probe_swallow_filtered":  {0, true},
	"probe_swallow_rethrown":  {0, true},
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
	got := map[string]map[string]string{}
	swallowGot := map[string]struct {
		calls int
		susp  bool
	}{}
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
		if strings.HasPrefix(entry, "probe_swallow_") {
			swallowGot[entry] = struct {
				calls int
				susp  bool
			}{a.calls, a.susp}
		}
		if a.err != nil || bb.err != nil || r.err != nil {
			t.Errorf("%s: a run failed outright (A=%v B=%v replay=%v), so its rows measure nothing", entry, a.err, bb.err, r.err)
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
		if len(ks) > 0 {
			got[entry] = map[string]string{}
		}
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
			got[entry][k] = verdict
			fmt.Fprintf(&report, "  %-34s %-50s | A=%q B=%q C=%q R=%q\n", k, verdict, trunc(a.result[k]), trunc(bb.result[k]), trunc(c.result[k]), trunc(r.result[k]))
		}
	}
	t.Log(report.String())
	for entry, rows := range probeWant {
		for k, want := range rows {
			g, ok := got[entry][k]
			if !ok {
				t.Errorf("%s / %s: no measurement (the probe no longer reports this row)", entry, k)
			} else if !probeVerdictMatches(g, want) {
				t.Errorf("%s / %s: measured %q, the vet is written against %q", entry, k, g, want)
			}
		}
		for k := range got[entry] {
			if _, ok := rows[k]; !ok {
				t.Errorf("%s / %s: a measured row nobody wrote a verdict for; add it to probeWant (and decide what the vet does with it)", entry, k)
			}
		}
	}
	for entry := range got {
		if _, ok := probeWant[entry]; !ok {
			t.Errorf("%s: an entry with measured rows and no row in probeWant", entry)
		}
	}
	for entry, w := range probeSwallowWant {
		g, ok := swallowGot[entry]
		if !ok {
			t.Errorf("%s: not measured", entry)
		} else if g != w {
			t.Errorf("%s: service calls=%d, engine saw suspension=%v; the vet's CV101 is written against calls=%d suspension=%v", entry, g.calls, g.susp, w.calls, w.susp)
		}
	}
	if os.Getenv("CLEAT_PROBE_REPORT") != "" {
		_ = os.WriteFile(os.Getenv("CLEAT_PROBE_REPORT"), []byte(report.String()), 0o644)
	}
}

// probeVerdictMatches reports whether the measured verdict starts with one of the "|"-separated alternatives in want.
func probeVerdictMatches(got, want string) bool {
	for _, alt := range strings.Split(want, "|") {
		if strings.HasPrefix(got, alt) {
			return true
		}
	}
	return false
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
