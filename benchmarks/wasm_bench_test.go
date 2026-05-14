// Package benchmarks provides performance benchmarks for the cleat durable
// workflow engine's WASM runtime. These benchmarks measure the cost of
// compiling, instantiating, and executing WASM modules via the internal
// host.Runtime and host.Engine, capturing realistic end-to-end overhead.
//
// Tier 1 benchmarks are always runnable with no external tooling.
// Tier 2 benchmarks require tinygo and will skip gracefully if unavailable.
//
// Usage:
//
//	go test -bench=. -benchmem -benchtime=10s ./benchmarks/
package benchmarks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// Tier 1 — always runnable (no external dependencies)
// ---------------------------------------------------------------------------

// BenchmarkCompilationInstantiation measures the cost of creating a Runtime,
// compiling a minimal WASM module, and instantiating it. This captures the
// overhead of wazero's compilation pipeline (validation + compilation to
// native code) and module instantiation (memory allocation + export resolution).
func BenchmarkCompilationInstantiation(b *testing.B) {
	ctx := context.Background()
	wasmBytes := minimalWasm()

	rt, err := host.NewRuntime(ctx)
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		compiled, err := rt.CompileModule(ctx, wasmBytes)
		if err != nil {
			b.Fatalf("CompileModule: %v", err)
		}
		mod, err := rt.InstantiateModule(ctx, compiled)
		if err != nil {
			compiled.Close(ctx)
			b.Fatalf("InstantiateModule: %v", err)
		}
		// Cleanup is part of the measured cycle.
		mod.Close(ctx)
		compiled.Close(ctx)
	}
}

// BenchmarkPayloadRoundTrip measures the round-trip cost of writing a 64 KB
// payload into WASM linear memory, calling a no-op exported function, and
// reading the data back. This captures memory write overhead, ABI function
// call overhead (crossing the WASM/native boundary), and memory read overhead.
func BenchmarkPayloadRoundTrip(b *testing.B) {
	ctx := context.Background()
	wasmBytes := minimalWasmWithMemory()

	rt, err := host.NewRuntime(ctx)
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		b.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		b.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	// The hand-crafted module has no _start, so InitModule is a no-op.

	// Prepare 64 KB of test data.
	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i)
	}

	mem := mod.Memory()

	// Guard: verify the module has at least 1 page of memory.
	if mem.Size() < 65536 {
		b.Fatalf("expected at least 64 KB memory, got %d bytes", mem.Size())
	}

	noopFn := mod.ExportedFunction("noop")
	if noopFn == nil {
		b.Fatal("expected noop export not found")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Write 64 KB into memory (offset 0).
		mem.Write(0, payload)

		// Call the no-op export through the ABI boundary.
		_, err := noopFn.Call(ctx, 0, 0, 65536, 65536)
		if err != nil {
			b.Fatalf("noop call failed: %v", err)
		}

		// Read the data back from memory.
		_, ok := mem.Read(0, 65536)
		if !ok {
			b.Fatal("failed to read memory")
		}
	}
}

// ---------------------------------------------------------------------------
// Tier 2 — require tinygo (skip gracefully if unavailable)
// ---------------------------------------------------------------------------

// BenchmarkEndToEndLatency measures the complete end-to-end latency of the
// durable workflow pipeline: compiling a testdata workflow to WASM (via
// buildBenchWasm), creating a Runtime, creating an Engine with a mock service
// caller, and executing the workflow. Each iteration of the benchmark loop
// runs the full Engine.Execute pipeline: compile, instantiate, init, and call.
func BenchmarkEndToEndLatency(b *testing.B) {
	wasmBytes := buildBenchWasm(b)
	ctx := context.Background()

	rt, err := host.NewRuntime(ctx)
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &benchCaller{}
	engine := host.NewEngine(rt, caller)

	input := json.RawMessage(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		result, _, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
		if err != nil {
			b.Fatalf("Execute: %v", err)
		}
		_ = result
	}
}

// BenchmarkFreshThroughput measures the throughput (workflows per second) of
// repeated fresh Engine.Execute calls. Each iteration includes the full cost
// of WASM compilation, instantiation, Go runtime init, execution, and cleanup.
func BenchmarkFreshThroughput(b *testing.B) {
	wasmBytes := buildBenchWasm(b)
	ctx := context.Background()

	rt, err := host.NewRuntime(ctx)
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &benchCaller{}
	engine := host.NewEngine(rt, caller)

	input := json.RawMessage(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		result, _, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
		if err != nil {
			b.Fatalf("Execute: %v", err)
		}
		_ = result
	}

	elapsed := time.Since(start)
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "wf/s")
	}
}

// BenchmarkReplayThroughput measures the throughput of deterministic replay.
// It first executes the workflow once to capture the event history, then
// repeatedly replays that history using Engine.Replay. Replay should be faster
// than fresh execution because cached history results are returned without
// making actual service calls.
func BenchmarkReplayThroughput(b *testing.B) {
	wasmBytes := buildBenchWasm(b)
	ctx := context.Background()

	// Step 1: Execute once to capture full event history.
	rtExecute, err := host.NewRuntime(ctx)
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer rtExecute.Close(ctx)

	caller := &benchCaller{}
	engineExecute := host.NewEngine(rtExecute, caller)

	input := json.RawMessage(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	result, history, _, _, _, err := engineExecute.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		b.Fatalf("initial Execute: %v", err)
	}
	if len(history) == 0 {
		b.Fatal("expected non-empty history for replay")
	}
	_ = result

	// Step 2: Create a fresh engine for replay and benchmark the replay loop.
	rtReplay, err := host.NewRuntime(ctx)
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	defer rtReplay.Close(ctx)

	engineReplay := host.NewEngine(rtReplay, caller)

	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		rResult, _, _, _, _, err := engineReplay.Replay(ctx, wasmBytes, "place_order", input, history)
		if err != nil {
			b.Fatalf("Replay: %v", err)
		}
		_ = rResult
	}

	elapsed := time.Since(start)
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "wf/s")
	}
}

// ---------------------------------------------------------------------------
// Mock service caller
// ---------------------------------------------------------------------------

// benchCaller is a minimal ServiceCaller that returns a generic JSON object
// for every call. Used by Tier 2 benchmarks to avoid external dependencies.
type benchCaller struct{}

func (c *benchCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	return `{}`, nil
}

// ---------------------------------------------------------------------------
// WASM bytecode helpers
// ---------------------------------------------------------------------------

// minimalWasm returns a valid but empty WASM module header (magic + version).
// It is sufficient for compile-and-instantiate benchmarking.
func minimalWasm() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic \0asm
		0x01, 0x00, 0x00, 0x00, // version 1
	}
}

// minimalWasmWithMemory returns a minimal valid WASM module containing:
//
//   - 1 page (64 KB) of linear memory, exported as "memory"
//   - A no-op function that accepts (i32, i32, i32, i32) and returns i64(0),
//     exported as "noop"
//
// The functype matches the convention used by CallExport: 4 × uint32 args
// (inputPtr, inputLen, outputPtr, outputMaxLen) returning an i64 status code.
func minimalWasmWithMemory() []byte {
	return []byte{
		// --- Magic + version -------------------------------------------------
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1

		// --- Type section (id=1) ---------------------------------------------
		// 1 functype: (i32, i32, i32, i32) -> i64
		// Content: count(1) + functype(60 04 params 01 result 7e) = 9 bytes
		0x01,                         // section id: Type
		0x09,                         // section size: 9 bytes
		0x01,                         // count: 1 type
		0x60,                         // functype
		0x04,                         // 4 parameter types
		0x7f, 0x7f, 0x7f, 0x7f,      // i32, i32, i32, i32
		0x01,                         // 1 result type
		0x7e,                         // i64

		// --- Function section (id=3) -----------------------------------------
		// 1 function, type index 0
		0x03,                         // section id: Function
		0x02,                         // section size: 2 bytes
		0x01,                         // count: 1 function
		0x00,                         // type index: 0

		// --- Memory section (id=5) -------------------------------------------
		// 1 memory, initial 1 page (64 KB), no maximum
		0x05,                         // section id: Memory
		0x03,                         // section size: 3 bytes
		0x01,                         // count: 1 memory
		0x00,                         // limits flag: no max
		0x01,                         // initial pages: 1

		// --- Export section (id=7) -------------------------------------------
		// 2 exports: "memory" -> mem 0, "noop" -> func 0
		0x07,                         // section id: Export
		0x11,                         // section size: 17 bytes
		0x02,                         // count: 2 exports
		// Export 1: "memory" (6 chars, kind=Memory(2), index=0)
		0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, // name "memory"
		0x02,                         // kind: Memory
		0x00,                         // index: 0
		// Export 2: "noop" (4 chars, kind=Function(0), index=0)
		0x04, 0x6e, 0x6f, 0x6f, 0x70, // name "noop"
		0x00,                         // kind: Function
		0x00,                         // index: 0

		// --- Code section (id=10) --------------------------------------------
		// 1 function body: i64.const 0, end
		0x0a,                         // section id: Code
		0x06,                         // section size: 6 bytes
		0x01,                         // count: 1 code entry
		0x04,                         // body size: 4 bytes (locals + expr)
		0x00,                         // 0 local groups
		0x42, 0x00,                   // i64.const 0
		0x0b,                         // end
	}
}

// buildBenchWasm compiles the testdata/basic workflow to WASM via the
// `durable build` pipeline with the tinygo target. It skips the benchmark
// if tinygo or the project root cannot be found.
func buildBenchWasm(b *testing.B) []byte {
	b.Helper()

	if _, err := exec.LookPath("tinygo"); err != nil {
		b.Skip("tinygo not installed — skipping WASM benchmark")
	}

	// Locate the project root by looking for cmd/durable/main.go.
	cwd, err := os.Getwd()
	if err != nil {
		b.Skip("cannot determine working directory")
	}
	projectRoot := cwd
	if _, err := os.Stat(filepath.Join(projectRoot, "cmd", "durable", "main.go")); os.IsNotExist(err) {
		projectRoot = filepath.Dir(cwd)
		if _, err := os.Stat(filepath.Join(projectRoot, "cmd", "durable", "main.go")); os.IsNotExist(err) {
			b.Skip("cannot find project root (cmd/durable/main.go)")
		}
	}

	tmpDir := b.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "durable"),
		"build", "--target", "tinygo", "-o", tmpDir,
		filepath.Join(projectRoot, "testdata", "basic"),
	)
	cmd.Dir = projectRoot

	// tinygo needs GOROOT and TINYGOROOT in its environment.
	cmd.Env = os.Environ()
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		cmd.Env = append(cmd.Env, "GOROOT="+goroot)
	}
	if tinygoGoroot := os.Getenv("DURABLE_TINYGO_GOROOT"); tinygoGoroot != "" {
		cmd.Env = append(cmd.Env, "GOROOT="+tinygoGoroot)
		cmd.Env = append(cmd.Env, "PATH="+tinygoGoroot+"/bin:"+os.Getenv("PATH"))
	}
	if tinygoroot := os.Getenv("TINYGOROOT"); tinygoroot != "" {
		cmd.Env = append(cmd.Env, "TINYGOROOT="+tinygoroot)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("durable build failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		b.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".wasm") {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				b.Fatalf("reading WASM file: %v", err)
			}
			return wasmBytes
		}
	}
	b.Fatalf("no .wasm file found in %s", tmpDir)
	return nil
}
