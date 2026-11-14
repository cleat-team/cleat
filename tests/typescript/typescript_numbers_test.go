package typescript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// The startup and replay numbers for the TypeScript guest, measured where the worker runs it: in-process, on the wasmtime
// backend the worker uses, with the compiled module cached. The Go twin of the same workflow is measured in the same run, in
// the same process, interleaved round by round, so machine drift lands on both and the ratio between them is the figure to
// read. THIS TEST RECORDS NUMBERS AND ASSERTS NONE OF THEM: tier 2 must run and may fail, and a pass here says only that the
// measurement happened, which the checks below are written to make true.
//
// What is measured, and what is not:
//   - module size as deployed;
//   - cold start: instantiate, one recorded host call, finish, on a module that is already compiled;
//   - the first run after deploy, which includes compilation (once per worker per module version);
//   - replay of a 1,000-event history to the suspension point, and a live execution of the same input for the ratio.
//   - NOT measured: peak guest memory, and throughput through a database. A Python twin is not built here either, so the
//     "cold start within 2x of Python" line has no Linux number from this test.
//
// The history is checked to hold at least the number of steps asked for, and the run to have suspended on the sleep. The
// first draft of this workload in the prototype used a host call that records nothing, measured a 0-event history, and
// reported every language as instantaneous; that guard is why the same mistake cannot pass here.

const (
	benchSteps  = 1000
	benchRounds = 3
	coldSamples = 30
	replaySamps = 10
	benchWF     = "00000000-0000-0000-0000-0000000b3c4d" // fixed: the engine derives the random stream from it, so Replay must match Execute
)

type nopCaller struct{}

func (nopCaller) Call(context.Context, string, string, string) (string, error) { return `{}`, nil }

func newBenchEngine(t *testing.T) (*engine.Engine, func()) {
	t.Helper()
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	e := engine.NewEngine(rt, nopCaller{},
		engine.WithWorkflowID(benchWF),
		engine.WithDefName("bench"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)
	return e, func() { _ = rt.Close(ctx) }
}

// benchInput is the workflow's input. Go wraps its parameter under the parameter's name; TypeScript takes the object flat.
func benchInput(lang string, n, sleepMs int) json.RawMessage {
	if lang == "go" {
		return json.RawMessage(fmt.Sprintf(`{"in":{"n":%d,"sleep_ms":%d}}`, n, sleepMs))
	}
	return json.RawMessage(fmt.Sprintf(`{"n":%d,"sleep_ms":%d}`, n, sleepMs))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func medianF(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// langRun is one language's module and what was measured of it.
type langRun struct {
	lang, entry        string
	wasm               []byte
	coldMed, replayMed []float64 // per-round medians, ms
	liveMed            []float64
	firstEver          float64
	eventsInHistory    int
	eventsPerColdRun   int
	suspendedOn        string
}

func (l *langRun) round(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	e, closeRT := newBenchEngine(t)
	defer closeRT()

	cold := benchInput(l.lang, 1, 0)
	t0 := time.Now()
	if _, _, _, _, _, err := e.Execute(ctx, l.wasm, l.entry, cold); err != nil {
		t.Fatalf("%s: first execution: %v", l.lang, err)
	}
	if l.firstEver == 0 {
		l.firstEver = ms(time.Since(t0))
	}
	for i := 0; i < 3; i++ { // warm the compile cache and the allocator; not timed
		if _, _, _, _, _, err := e.Execute(ctx, l.wasm, l.entry, cold); err != nil {
			t.Fatalf("%s: warm-up: %v", l.lang, err)
		}
	}
	var d []time.Duration
	for i := 0; i < coldSamples; i++ {
		t1 := time.Now()
		_, h, _, _, _, err := e.Execute(ctx, l.wasm, l.entry, cold)
		if err != nil {
			t.Fatalf("%s: cold run: %v", l.lang, err)
		}
		d = append(d, time.Since(t1))
		if i == 0 {
			l.eventsPerColdRun = len(h)
		}
	}
	l.coldMed = append(l.coldMed, ms(median(d)))

	in := benchInput(l.lang, benchSteps, 60000)
	_, hist, susp, _, _, err := e.Execute(ctx, l.wasm, l.entry, in)
	if err != nil {
		t.Fatalf("%s: executing the %d-step workflow: %v", l.lang, benchSteps, err)
	}
	if susp == nil {
		t.Fatalf("%s: the %d-step workflow did not suspend, so no sleep was reached", l.lang, benchSteps)
	}
	if len(hist) < benchSteps {
		t.Fatalf("%s: the history holds %d events for %d steps: the workload recorded less than it was asked to", l.lang, len(hist), benchSteps)
	}
	l.eventsInHistory, l.suspendedOn = len(hist), susp.Reason

	var live []time.Duration
	for i := 0; i < replaySamps; i++ { // a fresh execution of the same input, for the ratio of replay to live
		t1 := time.Now()
		if _, _, _, _, _, err := e.Execute(ctx, l.wasm, l.entry, in); err != nil {
			t.Fatalf("%s: live run: %v", l.lang, err)
		}
		live = append(live, time.Since(t1))
	}
	l.liveMed = append(l.liveMed, ms(median(live)))

	var rp []time.Duration
	for i := 0; i < replaySamps; i++ {
		t1 := time.Now()
		_, _, s2, _, _, err := e.Replay(ctx, l.wasm, l.entry, in, hist)
		if err != nil {
			t.Fatalf("%s: replay: %v", l.lang, err)
		}
		if s2 == nil {
			t.Fatalf("%s: replay %d did not reach the suspension point", l.lang, i)
		}
		rp = append(rp, time.Since(t1))
	}
	l.replayMed = append(l.replayMed, ms(median(rp)))
}

func (l *langRun) summary() map[string]any {
	span := func(v []float64) map[string]float64 {
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		return map[string]float64{"median_of_rounds_ms": medianF(v), "best_round_ms": s[0], "worst_round_ms": s[len(s)-1]}
	}
	return map[string]any{
		"module_bytes":                    len(l.wasm),
		"cold_start_cached":               span(l.coldMed),
		"first_run_includes_compile_ms":   l.firstEver,
		"replay_of_history":               span(l.replayMed),
		"live_execution_of_same_input":    span(l.liveMed),
		"events_in_history":               l.eventsInHistory,
		"events_recorded_per_cold_run":    l.eventsPerColdRun,
		"history_run_suspended_on_reason": l.suspendedOn,
	}
}

// buildGoTwin builds testdata/gobench the way a user's Go build does and returns the module.
func buildGoTwin(t *testing.T, root string) []byte {
	t.Helper()
	bin := t.TempDir()
	cleatBin := filepath.Join(bin, "cleat")
	goBuild(t, root, cleatBin, "./cmd/cleat")
	out := t.TempDir()
	cmd := exec.Command(cleatBin, "build", "-o", out, "./tests/typescript/testdata/gobench/")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build of the Go twin: %v\n%s", err, b)
	}
	found, _ := filepath.Glob(filepath.Join(out, "*.wasm"))
	if len(found) != 1 {
		t.Fatalf("cleat build wrote %d .wasm files (%v), want exactly 1", len(found), found)
	}
	b, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTypeScriptStartupAndReplayNumbers(t *testing.T) {
	root := needToolchain(t)

	res := build(t, root, "tests/typescript/testdata/workflows/bench.ts", "bench_steps")
	if res.exit != 0 {
		t.Fatalf("building the TypeScript bench module: build.sh exited %d:\n%s", res.exit, res.output)
	}
	tsWasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	ts := &langRun{lang: "ts", entry: "bench_steps", wasm: tsWasm}
	gw := &langRun{lang: "go", entry: "bench_steps", wasm: buildGoTwin(t, root)}

	// Interleaved: each round runs both languages, so a busy machine slows both alike. The spread across rounds is reported
	// beside the median, because a number from a shared runner is indicative and the spread says how much.
	for r := 0; r < benchRounds; r++ {
		ts.round(t)
		gw.round(t)
	}

	out := map[string]any{
		"what":       "TypeScript guest (Javy static mode) vs its Go twin, in-process on the wasmtime backend, compiled module cached",
		"rounds":     benchRounds,
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"cpus":       runtime.NumCPU(),
		"go":         runtime.Version(),
		"typescript": ts.summary(),
		"go_twin":    gw.summary(),
		"ratios_typescript_over_go": map[string]float64{
			"replay_of_history": medianF(ts.replayMed) / medianF(gw.replayMed),
			"cold_start_cached": medianF(ts.coldMed) / medianF(gw.coldMed),
			"module_bytes":      float64(len(ts.wasm)) / float64(len(gw.wasm)),
		},
		"not_measured": "peak guest memory; database throughput; a Python twin (so no Linux figure for the cold-start-vs-Python line)",
	}
	b, _ := json.MarshalIndent(out, "", " ")
	t.Logf("startup and replay numbers:\n%s", b)
	if p := os.Getenv("TS_NUMBERS_OUT"); p != "" {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
}
