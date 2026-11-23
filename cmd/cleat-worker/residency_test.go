package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// cleat#4162. Residency is ON by default (8 runs) and a refusal is always safe, so the failures that matter here are the ones that make a flag do nothing or a
// drain wait on a sleep, and neither is visible to a test that only reads the policy:
//
//  1. the flags, at their stock values, give a policy that is ON with 8 runs and carries the conservative numbers, and --resident-max-runs 0 turns it OFF, and a different flag value DOES change the policy
//     (so (1) is not satisfied by a function that ignores the flags);
//  2. main() calls the function that builds the policy, unconditionally (the shape of cleat#4212's check);
//  3. the drain paths send held runs back: a held run is in flight, so a drain that waited for it would wait for its wake.

func TestStockFlagsGiveAResidencyPolicyThatIsOnWithACountCapAndAMemoryBudget(t *testing.T) {
	useFakeHostMemory(t, 2<<30)
	for _, n := range []string{"resident-max-runs", "resident-memory-percent", "resident-memory-budget-mb", "resident-window-floor", "resident-window-cap",
		"resident-replay-multiplier", "resident-replay-fixed", "resident-replay-per-event"} {
		stockFlag(t, n)
	}
	cfg, err := residencyConfigFromFlags()
	if err != nil {
		t.Fatalf("the stock flags are refused: %v", err)
	}
	if cfg.MaxRuns != 1024 {
		t.Errorf("a stock worker holds MaxRuns = %d runs, want 1024 (cleat#4162: on by default; cleat#4377: the count cap, with the memory budget the usual limit)", cfg.MaxRuns)
	}
	// 20% of a 2 GiB worker is 409.6 MiB, rounded down. The value is the default percentage reaching the policy: a stock worker's budget is a share of ITS memory.
	if want := uint64(429496729); cfg.MemoryBudgetBytes != want {
		t.Errorf("a stock worker on a 2 GiB cgroup limit has a residency budget of %d bytes, want %d (20%% of the limit; cleat#4377)", cfg.MemoryBudgetBytes, want)
	}
	// The CONTROL that makes the row above mean something: the documented way to turn it off must still work, or "on by default" is "on, and you cannot leave".
	setFlag(t, "resident-max-runs", "0")
	t.Cleanup(func() { stockFlag(t, "resident-max-runs") })
	off, err := residencyConfigFromFlags()
	if err != nil {
		t.Fatalf("--resident-max-runs 0 is refused: %v", err)
	}
	if off.MaxRuns != 0 {
		t.Errorf("--resident-max-runs 0 gives MaxRuns = %d, want 0 (residency off)", off.MaxRuns)
	}
	stockFlag(t, "resident-max-runs")
	if cfg.WindowFloor != 10*time.Second || cfg.WindowCap != 5*time.Minute || cfg.ReplayMultiplier != 10 ||
		cfg.ReplayFixed != 500*time.Millisecond || cfg.ReplayPerEvent != 0 {
		t.Errorf("the stock policy is %+v, want floor 10s, cap 5m, multiplier 10, fixed 500ms, per-event 0", cfg)
	}
}

func TestEveryResidencyFlagChangesThePolicy(t *testing.T) {
	useFakeHostMemory(t, 2<<30)
	// The control for the stock row: a flag value that is not the default must reach the policy, or the stock row is also true of a function that
	// ignores every flag.
	for _, c := range []struct {
		flag, value string
		check       func(c *residencyView) bool
	}{
		{"resident-max-runs", "7", func(c *residencyView) bool { return c.maxRuns == 7 }},
		{"resident-memory-budget-mb", "96", func(c *residencyView) bool { return c.budget == 96<<20 }},
		{"resident-memory-percent", "10", func(c *residencyView) bool { return c.budget == 214748364 }},
		{"resident-window-floor", "3s", func(c *residencyView) bool { return c.floor == 3*time.Second }},
		{"resident-window-cap", "2m", func(c *residencyView) bool { return c.cap == 2*time.Minute }},
		{"resident-replay-multiplier", "4", func(c *residencyView) bool { return c.mult == 4 }},
		{"resident-replay-fixed", "1s", func(c *residencyView) bool { return c.fixed == time.Second }},
		{"resident-replay-per-event", "2ms", func(c *residencyView) bool { return c.perEvent == 2*time.Millisecond }},
	} {
		setFlag(t, c.flag, c.value)
		cfg, err := residencyConfigFromFlags()
		if err != nil {
			t.Errorf("--%s=%s was refused: %v", c.flag, c.value, err)
			continue
		}
		v := &residencyView{cfg.MaxRuns, cfg.MemoryBudgetBytes, cfg.WindowFloor, cfg.WindowCap, cfg.ReplayMultiplier, cfg.ReplayFixed, cfg.ReplayPerEvent}
		if !c.check(v) {
			t.Errorf("--%s=%s did not reach the policy: %+v", c.flag, c.value, *v)
		}
		stockFlag(t, c.flag)
	}
}

type residencyView struct {
	maxRuns         int
	budget          uint64
	floor, cap      time.Duration
	mult            float64
	fixed, perEvent time.Duration
}

// --poll IS the recheck interval of a held signal wait (cleat#4162, 2b-i), which is what makes "a held run is never later than the same run with residency off" true. The stock value
// equals the engine's default, so the stock row alone is also true of a function that ignores the flag: the control is a value that is not the default.
func TestThePollFlagIsTheRecheckIntervalOfAHeldSignalWait(t *testing.T) {
	useFakeHostMemory(t, 2<<30)
	stockFlag(t, "poll")
	defer stockFlag(t, "poll")
	cfg, err := residencyConfigFromFlags()
	if err != nil {
		t.Fatalf("the stock flags are refused: %v", err)
	}
	if cfg.RecheckInterval != 500*time.Millisecond {
		t.Errorf("the stock recheck interval is %s, want 500ms (the stock --poll)", cfg.RecheckInterval)
	}
	setFlag(t, "poll", "250ms")
	cfg, err = residencyConfigFromFlags()
	if err != nil {
		t.Fatalf("--poll=250ms was refused: %v", err)
	}
	if cfg.RecheckInterval != 250*time.Millisecond {
		t.Errorf("--poll=250ms gave a recheck interval of %s: the flag does not reach the policy, so a held run would recheck at a different cadence than residency-off waits for", cfg.RecheckInterval)
	}
	// A value that is not an interval is not used, and must not refuse the boot of a worker that does not use residency.
	for _, v := range []string{"0s", "-1s"} {
		setFlag(t, "poll", v)
		cfg, err = residencyConfigFromFlags()
		if err != nil {
			t.Errorf("--poll=%s refused the boot through the residency policy: %v", v, err)
			continue
		}
		if cfg.RecheckInterval != 500*time.Millisecond {
			t.Errorf("--poll=%s gave a recheck interval of %s, want the 500ms default", v, cfg.RecheckInterval)
		}
	}
}

func TestAResidencyFlagThatCannotMeanWhatItSaysIsRefused(t *testing.T) {
	useFakeHostMemory(t, 2<<30)
	for _, c := range []struct{ flag, value string }{
		{"resident-max-runs", "-1"},
		{"resident-memory-percent", "-1"},
		{"resident-memory-percent", "100.5"},
		{"resident-memory-percent", "NaN"},
		{"resident-memory-budget-mb", "-1"},
		{"resident-window-cap", "1s"}, // below the 10s floor
		{"resident-replay-multiplier", "0"},
		{"resident-replay-fixed", "-1s"},
	} {
		setFlag(t, c.flag, c.value)
		if _, err := residencyConfigFromFlags(); err == nil {
			t.Errorf("--%s=%s was accepted", c.flag, c.value)
		}
		stockFlag(t, c.flag)
	}
}

func TestMainBuildsTheResidencyPolicyFromTheFlags(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Known positive: the real main.go. A scan that matched nothing could not pass this.
	if !callsOnTheLeg(t, string(src), "residencyConfigFromFlags")["residencyConfigFromFlags"] {
		t.Error("main() does not call residencyConfigFromFlags from one of its own top-level statements: the residency flags would parse and change nothing")
	}
	if got := callsOnTheLeg(t, "package main\nfunc main() {\nif len(os.Args) > 1 { residencyConfigFromFlags() }\n}\n", "residencyConfigFromFlags"); got["residencyConfigFromFlags"] {
		t.Error("scan control: a call inside an if was reported as unconditional")
	}
}

// callsInMethod reports whether the named method of Worker contains a call to a method called callee on w.resident, at any depth.
func callsInMethod(t *testing.T, src, method, callee string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "setup.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != method || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != callee {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "resident" {
				found = true
			}
			return true
		})
	}
	return found
}

func TestEveryPathThatDrainsTheWorkerSendsTheHeldRunsBack(t *testing.T) {
	raw, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatalf("read setup.go: %v", err)
	}
	src := string(raw)
	// gracefulShutdown is SIGTERM; the dispatch loop's draining branch is the cordon (POST /api/admin/drain), which drains without gracefulShutdown. A held
	// run is in w.inflight, so either drain that forgot to send it back would wait for its wake.
	for _, m := range []struct{ method, callee string }{
		{"gracefulShutdown", "Drain"},
		{"dispatchLoop", "Drain"},
		{"dispatchLoop", "EvictAll"},
	} {
		// Known positive first: the method must exist, or "no call" is what a scan of nothing reports.
		if !strings.Contains(src, "func (w *Worker) "+m.method+"(") {
			t.Fatalf("setup.go has no method %s: this test reads the wrong thing (rename it here)", m.method)
		}
		if !callsInMethod(t, src, m.method, m.callee) {
			t.Errorf("Worker.%s does not call w.resident.%s: a held run would outlive the drain or the memory hard limit", m.method, m.callee)
		}
	}
	// Negative control: the same scan over a method that does not call it must say so.
	if callsInMethod(t, "package main\ntype Worker struct{}\nfunc (w *Worker) gracefulShutdown() { w.draining.Store(true) }\n", "gracefulShutdown", "Drain") {
		t.Error("scan control: a method with no w.resident.Drain call was reported as having one")
	}
}

// handsTheHandleWhenEnabled reports whether src has an `if w.resident.Enabled() { ... engine.WithResidency(...) ... }`: the condition must be exactly that
// call, not an expression that merely contains it, so a condition disabled with `false &&` (which leaves the text in place) is not mistaken for the real one.
func handsTheHandleWhenEnabled(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "setup.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	isEnabledCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Enabled" {
			return false
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		return ok && inner.Sel.Name == "resident"
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !isEnabledCall(ifs.Cond) {
			return true
		}
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithResidency" {
					found = true
				}
			}
			return true
		})
		return true
	})
	return found
}

func TestEveryExecutionIsGivenItsResidencyHandleWhenResidencyIsOn(t *testing.T) {
	raw, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatalf("read setup.go: %v", err)
	}
	src := string(raw)
	if !handsTheHandleWhenEnabled(t, src) {
		t.Error("no `if w.resident.Enabled() { ... engine.WithResidency(...) }` in setup.go: the flags would turn residency on for a worker whose runs never park")
	}
	// Negative controls: the scan must say "no" for each way the condition can stop being the real one, or the check above decides nothing.
	for name, body := range map[string]string{
		"a condition disabled with false &&": `if false && w.resident.Enabled() { opts = append(opts, engine.WithResidency(h)) }`,
		"no condition at all":                `opts = append(opts, engine.WithResidency(h))`,
		"the handle in the else branch":      `if w.resident.Enabled() { _ = 1 } else { opts = append(opts, engine.WithResidency(h)) }`,
	} {
		if handsTheHandleWhenEnabled(t, "package main\nfunc (w *Worker) f() {\n"+body+"\n}\n") {
			t.Errorf("scan control %q was reported as the real wiring", name)
		}
	}
	// The handle must be built BEFORE the engine is: an option appended after NewEngine configures nothing.
	handle := strings.Index(src, "engine.WithResidency(w.resident.ForRun(")
	build := strings.Index(src, "eng := engine.NewEngine(nil, caller, engineOpts...)")
	if handle < 0 || build < 0 || handle > build {
		t.Errorf("WithResidency is at %d and NewEngine at %d: the option must be appended before the engine is built", handle, build)
	}
}
