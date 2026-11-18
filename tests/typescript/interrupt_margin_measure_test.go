package typescript

// cleat#3922: the measurement both interrupt margins are derived from.
//
// The soft kill stops a runaway guest by answering 1 from cleat_interrupt_requested once the budget
// it has left is at or below a reserve. Too small and the guest reaches the hard trap before the
// handler is asked again -- the very trap the soft kill exists to avoid. So each reserve has to be
// at least the largest gap the guest can run between two consecutive handler calls.
//
// TWO ARMS, because they cover different configurations:
//   - fuel:  fires when remaining fuel is at or below DefaultWasmtimeInterruptMarginFuel. Only
//     active when --wasm-instruction-limit is set; the flag defaults to 0.
//   - epoch: fires when the guest's remaining INSTANCE TIMEOUT is at or below
//     DefaultWasmtimeInterruptMarginTime. Always active, and the only arm a default operator has.
//
// The constants are NOT taken from this file: run it, and put what it prints into the defaults.
// Each workload loops for ever; the run ends on the arm under test, so what is measured is the
// observation cadence, not the policy.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func TestMeasureInterruptMargin(t *testing.T) {
	// A measurement harness, not a regression test: twelve runs of a 2s (or 400M fuel) endless loop,
	// about a minute. It is skipped by default because the TypeScript tier-2 job already runs close
	// to go test's 10-minute default (cleat#4051), and nothing here can fail in a way that says
	// anything about the tree. Re-run it when a margin needs re-deriving.
	if os.Getenv("CLEAT_MEASURE_INTERRUPT") == "" {
		t.Skip("set CLEAT_MEASURE_INTERRUPT=1 to re-derive the interrupt margins (about a minute)")
	}
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/interrupt_workloads.ts"
	entries := []string{"spin_loop", "spin_regex", "spin_native", "spin_json", "spin_string", "spin_string_big"}
	res := build(t, root, src, "spin_loop,spin_regex,spin_json,spin_string,spin_string_big,spin_native,spin_string_probe")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasmBytes, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}

	modes := []struct {
		name string
		opts []engine.WasmtimeOption
	}{
		{"fuel", []engine.WasmtimeOption{engine.WithWasmtimeInstructionLimit(400_000_000)}},
		// No instruction limit: this is the default configuration, where the epoch fence is the kill.
		{"epoch", []engine.WasmtimeOption{engine.WithWasmtimeExecutionTimeout(2 * time.Second)}},
	}

	for _, entry := range entries {
		for _, mode := range modes {
			t.Run(entry+"/"+mode.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				rt, err := engine.NewRuntime(ctx, 0, 0)
				if err != nil {
					t.Fatalf("engine.NewRuntime: %v", err)
				}
				defer func() { _ = rt.Close(ctx) }()
				wt, err := engine.NewWasmtimeBackend(ctx, mode.opts...)
				if err != nil {
					t.Fatalf("engine.NewWasmtimeBackend: %v", err)
				}
				caller := &recordingCaller{}
				e := engine.NewEngine(rt, caller,
					engine.WithWorkflowID("wf-margin-"+entry),
					engine.WithDefName(entry),
					engine.WithDefVersion(1),
					engine.WithBackends(engine.WasmtimeLanguages, wt),
				)
				engine.MeasureInterrupt()
				start := time.Now()
				_, _, _, _, _, execErr := e.Execute(ctx, wasmBytes, entry, []byte(`{}`))
				elapsed := time.Since(start)
				polls, maxGapFuel, maxGapTime, maxUnwind, maxUnwindFuel := engine.InterruptStats()
				t.Logf("%s/%s: polls=%d maxFuelBetweenPolls=%d maxTimeBetweenPolls=%s maxUnwind=%s maxUnwindFuel=%d elapsed=%s callerSaw=%v err=%s",
					entry, mode.name, polls, maxGapFuel, maxGapTime, maxUnwind, maxUnwindFuel, elapsed.Round(time.Millisecond), caller.snapshot(), firstLine(execErr))
				if polls == 0 {
					t.Logf("%s/%s: THE HANDLER WAS NEVER CALLED (gap is at least the whole %s run)", entry, mode.name, elapsed.Round(time.Millisecond))
				}
			})
		}
	}
}

func firstLine(err error) string {
	if err == nil {
		return "<nil>"
	}
	s := err.Error()
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
