//go:build cgo

// Phase 4 (plan section 5), in-process half: cold start, replay of a long history, and peak guest memory, for one module per
// run. The engine, backend and stores are the ones cleat/wasmtest builds, except that this constructs the wasmtime backend itself
// so a memory limit can be set. Prints one JSON object. The database-backed half (row size, throughput) is phase4_db.py.
//
//	go run ./bench/harness -wasm .work/bench/ts.wasm -mode cold   -samples 50
//	go run ./bench/harness -wasm .work/bench/ts.wasm -mode replay -steps 1000 -samples 10
//	go run ./bench/harness -wasm .work/bench/ts.wasm -mode mem    -steps 1000
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

type nopCaller struct{}

func (nopCaller) Call(context.Context, string, string, string) (string, error) { return `{}`, nil }

const workflowID = "00000000-0000-0000-0000-0000000b3c4d" // fixed: the engine derives the random stream from it, so Replay must match Execute

func newEngine(memBytes int64) (*engine.Engine, func()) {
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	must(err)
	var wopts []engine.WasmtimeOption
	if memBytes > 0 {
		wopts = append(wopts, engine.WithWasmtimeMemoryLimits(memBytes, 0, 0))
	}
	wt, err := engine.NewWasmtimeBackend(ctx, wopts...)
	must(err)
	e := engine.NewEngine(rt, nopCaller{},
		engine.WithSignalStore(wasmtest.NewInMemorySignalStore()),
		engine.WithPromiseStore(wasmtest.NewInMemoryPromiseStore()),
		engine.WithWorkflowState(wasmtest.NewTestWorkflowState()),
		engine.WithWorkflowID(workflowID),
		engine.WithDefName("bench"),
		engine.WithChildWorkflowStore(wasmtest.NewInMemoryChildWorkflowStore()),
		engine.WithConcurrencyKeyStore(wasmtest.NewInMemoryConcurrencyKeyStore()),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)
	return e, func() { _ = rt.Close(ctx) }
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "UNMEASURED:", err)
		os.Exit(2)
	}
}

func input(n, sleepMs int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"n":%d,"sleep_ms":%d}`, n, sleepMs))
}

// Go wraps its parameter under the parameter name; the others take the object flat.
func inputFor(lang string, n, sleepMs int) json.RawMessage {
	if lang == "go" {
		return json.RawMessage(fmt.Sprintf(`{"in":{"n":%d,"sleep_ms":%d}}`, n, sleepMs))
	}
	return input(n, sleepMs)
}

func stats(d []time.Duration) map[string]float64 {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000 }
	var sum time.Duration
	for _, x := range s {
		sum += x
	}
	return map[string]float64{"n": float64(len(s)), "min_ms": ms(s[0]), "median_ms": ms(s[len(s)/2]), "p95_ms": ms(s[(len(s)*95)/100]), "max_ms": ms(s[len(s)-1]), "mean_ms": ms(sum) / float64(len(s))}
}

func main() {
	wasm := flag.String("wasm", "", "module")
	lang := flag.String("lang", "", "go|ts|rust|python (names the input shape)")
	mode := flag.String("mode", "cold", "cold|replay|mem")
	samples := flag.Int("samples", 30, "timed samples")
	steps := flag.Int("steps", 1000, "recorded host calls for replay/mem")
	entry := flag.String("entry", "bench_steps", "entry point")
	flag.Parse()
	b, err := os.ReadFile(*wasm)
	must(err)
	if *lang == "" {
		must(fmt.Errorf("-lang is required"))
	}
	ctx := context.Background()
	out := map[string]any{"lang": *lang, "mode": *mode, "wasm_bytes": len(b)}

	switch *mode {
	case "cold":
		e, closeRT := newEngine(0)
		defer closeRT()
		in := inputFor(*lang, 1, 0)
		t0 := time.Now()
		_, _, _, _, _, err := e.Execute(ctx, b, *entry, in)
		must(err)
		out["first_ever_ms_includes_compile"] = float64(time.Since(t0).Microseconds()) / 1000
		for i := 0; i < 3; i++ { // warm the cache and the allocator; not timed
			_, _, _, _, _, err = e.Execute(ctx, b, *entry, in)
			must(err)
		}
		var d []time.Duration
		for i := 0; i < *samples; i++ {
			t := time.Now()
			_, h, _, _, _, err := e.Execute(ctx, b, *entry, in)
			must(err)
			d = append(d, time.Since(t))
			if i == 0 {
				out["events_recorded_per_run"] = len(h)
			}
		}
		out["cached"] = stats(d)

	case "replay":
		e, closeRT := newEngine(0)
		defer closeRT()
		in := inputFor(*lang, *steps, 60000)
		t := time.Now()
		_, hist, susp, _, _, err := e.Execute(ctx, b, *entry, in)
		must(err)
		out["execute_first_ms_includes_compile"] = float64(time.Since(t).Microseconds()) / 1000
		if susp == nil {
			must(fmt.Errorf("the %d-step workflow did not suspend (no sleep reached)", *steps))
		}
		out["events_in_history"] = len(hist)
		out["suspended_on"] = susp.Reason
		if len(hist) < *steps {
			must(fmt.Errorf("history has %d events for %d steps", len(hist), *steps))
		}
		var ex []time.Duration
		for i := 0; i < *samples; i++ { // a fresh execution of the same input, for the ratio of replay to live
			t := time.Now()
			_, _, _, _, _, err := e.Execute(ctx, b, *entry, in)
			must(err)
			ex = append(ex, time.Since(t))
		}
		out["execute"] = stats(ex)
		var rp []time.Duration
		for i := 0; i < *samples; i++ {
			t := time.Now()
			_, _, s2, _, _, err := e.Replay(ctx, b, *entry, in, hist)
			must(err)
			if s2 == nil {
				must(fmt.Errorf("replay %d did not reach the suspension point", i))
			}
			rp = append(rp, time.Since(t))
		}
		out["replay"] = stats(rp)

	case "mem":
		// Peak linear memory is the smallest limit under which the 1,000-step replay still succeeds (the engine has no gauge;
		// wasm linear memory only grows, so this is the high-water mark). Resolution 1 MiB up to the 32 MiB engine ceiling.
		in := inputFor(*lang, *steps, 60000)
		e0, c0 := newEngine(0)
		_, hist, susp, _, _, err := e0.Execute(ctx, b, *entry, in)
		c0()
		must(err)
		if susp == nil {
			must(fmt.Errorf("did not suspend"))
		}
		ok := func(mib int) (good bool, why string) {
			e, c := newEngine(int64(mib) << 20)
			defer c()
			_, _, s2, _, _, err := e.Replay(ctx, b, *entry, in, hist)
			if err != nil {
				return false, err.Error()
			}
			return s2 != nil, ""
		}
		lo, hi := 1, 32 // invariant: hi succeeds (checked), lo-1 fails or is below any possible size
		if g, why := ok(hi); !g {
			must(fmt.Errorf("replay fails even at the 32 MiB ceiling: %s", why))
		}
		probes := map[string]bool{}
		for lo < hi {
			mid := (lo + hi) / 2
			g, why := ok(mid)
			probes[fmt.Sprintf("%dMiB", mid)] = g
			if g {
				hi = mid
			} else {
				lo = mid + 1
				if len(why) > 0 {
					out["a_failing_probe_said"] = strings.SplitN(why, "\n", 2)[0]
				}
			}
		}
		// known-negative for the search itself: one MiB below the answer must fail, or the limit is not what bounded it
		if hi > 1 {
			if g, _ := ok(hi - 1); g {
				must(fmt.Errorf("replay succeeded at %d MiB after failing in the search; the probe is not monotonic", hi-1))
			}
		}
		out["peak_linear_memory_mib_upper_bound"] = hi
		out["probes"] = probes
	default:
		must(fmt.Errorf("unknown mode %q", *mode))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	must(enc.Encode(out))
}
