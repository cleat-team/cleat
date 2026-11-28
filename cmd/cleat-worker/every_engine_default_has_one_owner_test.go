package main

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A setting that has an engine default and a worker flag has ONE default, and it lives in the engine. The worker's flag takes it from there
// (`flag.Int("...", engine.DefaultX, ...)`), so the two cannot disagree. That was not true three times (cleat#4130: a payload limit of 64 KiB on the flag
// and 1 MiB in the engine, then another of 32 KiB against 1 MiB; and --wasm-instance-timeout, whose help said 0 disables it where the engine reads 0 as
// the default), and each time a document quoted whichever number its author had read, with every check passing, because each check compared a document
// with ONE of the two numbers.
//
// Three checks, each reading a different thing:
//
//  1. every exported `Default*` in the engine is either used by the worker or listed below with the reason it has no flag, so a new engine default that
//     no flag reaches (or a flag that kept its own literal copy) has to be decided, and an entry that stops being true is reported;
//  2. the flags that convert or restate an engine value (MB for bytes, ms for a Duration) are compared with it, by value;
//  3. a flag that DOES override an engine default says so here with BOTH values, so the override is a recorded decision, and fixing it (or changing
//     either number) fails the test until the entry is deleted or updated.

// engineDefaultsTheWorkerDoesNotUse lists every exported engine Default* that no worker source references, with why. Update the reason, not just the name.
var engineDefaultsTheWorkerDoesNotUse = map[string]string{
	"DefaultCatchUpLimit":                "a per-schedule setting (cron), set through the schedule API, not a worker flag",
	"DefaultCompactionThreshold":         "OVERRIDDEN by --compaction-threshold, deliberately (owner, 2026-11-19): see flagsThatOverrideAnEngineDefault (cleat#4171)",
	"DefaultDeferPassBudget":             "no worker flag: the engine's own budget for one defer pass (--wasm-defer-budget bounds the segment, not this)",
	"DefaultFlushRetryWindow":            "--flush-retry-window defaults to 0, which means \"use this\": the flag carries the sentinel, not a copy",
	"DefaultGCOffset":                    "no worker flag: version GC's own offset, reached through engine.DefaultGCOptions",
	"DefaultMaxCompactedEvents":          "no worker flag: a bound inside compaction itself",
	"DefaultMemoryLimitPages":            "the wazero Runtime's default (developer tooling); the worker's limit is DefaultWasmtimeMemoryLimitBytes",
	"DefaultScheduleTimezone":            "a per-schedule setting, set through the schedule API",
	"DefaultWasmtimeInstancesLimit":      "no worker flag: a wasmtime resource limit set by the engine",
	"DefaultWasmtimeInterruptMarginFuel": "no worker flag: an engine-internal margin for the interruption mechanism",
	"DefaultWasmtimeInterruptMarginTime": "no worker flag: an engine-internal margin for the interruption mechanism",
	"DefaultWasmtimeTableElementsLimit":  "no worker flag: a wasmtime resource limit set by the engine",
}

// flagsThatOverrideAnEngineDefault records the deliberate or not-yet-decided disagreements: the flag, the engine constant it disagrees with, and the
// two values as they are TODAY. The test fails when either value changes, so an override cannot be fixed, or made worse, without this list being touched.
var flagsThatOverrideAnEngineDefault = map[string]struct {
	engineName  string
	flagDefault string
	engineValue func() string
	was         string
	reason      string
}{
	"compaction-threshold": {"DefaultCompactionThreshold", "100", func() string { return strconv.Itoa(engine.DefaultCompactionThreshold) }, "1000",
		"every worker runs 100; the engine's constant says 1000 and its own comment says nothing reads it. DECIDED 2026-11-19 (cleat#4171): workers keep 100, a DELIBERATE override of the engine's 1000, which stays the number the engine's own tests use"},
}

// flagsThatRestateAnEngineValue are compared by value: the conversions (bytes to MB, a Duration to milliseconds) a reference alone would not prove.
var flagsThatRestateAnEngineValue = map[string]func() string{
	"wasm-instance-timeout":   func() string { return engine.DefaultWasmtimeExecutionTimeout.String() },
	"wasm-wall-clock-ceiling": func() string { return engine.DefaultWasmWallClockCeiling.String() },
	"wasm-memory-max-mb":      func() string { return strconv.FormatInt(engine.DefaultWasmtimeMemoryLimitBytes>>20, 10) },
	"wasm-max-string-len":     func() string { return strconv.Itoa(engine.DefaultMaxWasmStringLen) },
	"wasm-output-buffer-size": func() string { return strconv.Itoa(engine.DefaultOutBufSize) },
	"wasm-defer-budget":       func() string { return engine.DefaultWasmtimeDeferBudget.String() },
	"host-retry-budget":       func() string { return engine.DefaultHostRetryBudget.String() },
	"max-retries":             func() string { return strconv.Itoa(engine.MaxRetryAttempts) },
	"batch-flush-max-wait-ms": func() string { return strconv.FormatInt(engine.DefaultBatchFlushMaxWait.Milliseconds(), 10) },
	"batch-flush-max-size":    func() string { return strconv.Itoa(engine.DefaultBatchFlushMaxSize) },
	"batch-flush-enter-rate":  func() string { return strconv.Itoa(int(engine.DefaultBatchFlushEnterRate)) },
	"batch-flush-exit-rate":   func() string { return strconv.Itoa(int(engine.DefaultBatchFlushExitRate)) },
}

func engineDefaultNames(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../engine/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no engine sources found (err=%v): the scan is not seeing the engine", err)
	}
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, sp := range gd.Specs {
				for _, n := range sp.(*ast.ValueSpec).Names {
					if strings.HasPrefix(n.Name, "Default") && ast.IsExported(n.Name) {
						names[n.Name] = true
					}
				}
			}
		}
	}
	return names
}

func engineDefaultsTheWorkerReferences(t *testing.T) map[string]bool {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	used := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if se, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "engine" && strings.HasPrefix(se.Sel.Name, "Default") {
					used[se.Sel.Name] = true
				}
			}
			return true
		})
	}
	return used
}

// flagDeclarationsThatTakeTheirDefaultFromTheEngine returns, for every `flag.<Type>("name", <default>, ...)` call in the worker's sources, whether
// <default> mentions the engine package. A literal that happens to equal the engine's value today is a copy that will drift, and comparing values
// cannot see it (measured: reverting --wasm-instance-timeout to `30*time.Second` passed a value comparison).
func flagDeclarationsThatTakeTheirDefaultFromTheEngine(t *testing.T) map[string]bool {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			se, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := se.X.(*ast.Ident); !ok || id.Name != "flag" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			fromEngine := false
			ast.Inspect(call.Args[1], func(m ast.Node) bool {
				if sel, ok := m.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "engine" {
						fromEngine = true
					}
				}
				return true
			})
			out[name] = fromEngine
			return true
		})
	}
	return out
}

func TestEveryEngineDefaultIsUsedByTheWorkerOrSaysWhy(t *testing.T) {
	all := engineDefaultNames(t)
	// A scan that finds almost nothing is not a pass (25 on 2026-11-19: `go run` of the same AST walk).
	if len(all) < 20 {
		t.Fatalf("only %d exported engine Default* identifiers found; the scan is not seeing the engine", len(all))
	}
	used := engineDefaultsTheWorkerReferences(t)
	var undecided, stale []string
	for name := range all {
		if !used[name] {
			if _, ok := engineDefaultsTheWorkerDoesNotUse[name]; !ok {
				undecided = append(undecided, name)
			}
		}
	}
	for name := range engineDefaultsTheWorkerDoesNotUse {
		switch {
		case !all[name]:
			stale = append(stale, name+" (no longer exists in the engine)")
		case used[name]:
			stale = append(stale, name+" (the worker now uses it)")
		}
	}
	sort.Strings(undecided)
	sort.Strings(stale)
	if len(undecided) > 0 {
		t.Errorf("these engine defaults are not referenced by any worker source and are not listed in engineDefaultsTheWorkerDoesNotUse: %v.\n"+
			"A flag for the same setting must take its default from the engine constant (flag.X(\"...\", engine.%s, ...)) so the two cannot disagree; "+
			"if there is no flag, say why in the list.", undecided, undecided[0])
	}
	if len(stale) > 0 {
		t.Errorf("entries in engineDefaultsTheWorkerDoesNotUse that are no longer true: %v", stale)
	}
}

func TestFlagsThatRestateAnEngineValueHaveTheEnginesValue(t *testing.T) {
	declarations := flagDeclarationsThatTakeTheirDefaultFromTheEngine(t)
	for name, want := range flagsThatRestateAnEngineValue {
		f := flag.Lookup(name)
		if f == nil {
			t.Errorf("--%s is listed in flagsThatRestateAnEngineValue and the worker declares no such flag", name)
			continue
		}
		if f.DefValue != want() {
			t.Errorf("--%s defaults to %q and the engine's value is %q: a running worker enforces one and a document quotes the other", name, f.DefValue, want())
		}
		if fromEngine, declared := declarations[name]; !declared {
			t.Errorf("--%s: no flag.<Type>(%q, ...) declaration was found in the worker's sources; the scan is not seeing it", name, name)
		} else if !fromEngine {
			t.Errorf("--%s defaults to a literal that equals the engine's value today (%q). Take it from the engine (flag.X(%q, engine.<Default>, ...)) so it cannot drift", name, f.DefValue, name)
		}
	}
}

func TestAnOverrideOfAnEngineDefaultIsARecordedDecision(t *testing.T) {
	for name, o := range flagsThatOverrideAnEngineDefault {
		f := flag.Lookup(name)
		if f == nil {
			t.Errorf("--%s is listed in flagsThatOverrideAnEngineDefault and the worker declares no such flag", name)
			continue
		}
		gotEngine := o.engineValue()
		if f.DefValue == gotEngine {
			t.Errorf("--%s and engine.%s now agree (%q): delete the entry in flagsThatOverrideAnEngineDefault, and its line in engineDefaultsTheWorkerDoesNotUse if the flag now uses the constant",
				name, o.engineName, gotEngine)
			continue
		}
		if f.DefValue != o.flagDefault || gotEngine != o.was {
			t.Errorf("--%s defaults to %q and engine.%s is %q; the recorded decision says %q and %q (%s). One of the two numbers moved: decide again and update the entry",
				name, f.DefValue, o.engineName, gotEngine, o.flagDefault, o.was, o.reason)
		}
	}
	// The set of flags actually named by an override must be a set the engine-default list knows about.
	for name, o := range flagsThatOverrideAnEngineDefault {
		if _, ok := engineDefaultsTheWorkerDoesNotUse[o.engineName]; !ok {
			t.Errorf("--%s overrides engine.%s but that name is not in engineDefaultsTheWorkerDoesNotUse (the worker would have to be using it)", name, o.engineName)
		}
	}
}
