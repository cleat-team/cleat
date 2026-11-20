package main

import (
	"flag"
	"strconv"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4171 (owner decisions, 2026-11-19): a worker with NO flags set runs with a 256 MiB per-store memory limit and a 1 MiB output
// buffer. Until then the worker's flags said 32 MiB and 32 KiB while the engine and ABI.md said otherwise for the buffer, and changing a flag's
// default alone would not have reached the engine: the worker handed the backend a limit only when the flag was SET, so a stock worker ran on the
// engine's own constant. The point of this test is that what a stock worker is told is what it is meant to be told, read from the flag as
// config.go really declares it (flag.CommandLine) and passed through the same function main() uses.
func TestAStockWorkerRunsWithA256MiBMemoryLimit(t *testing.T) {
	f := flag.CommandLine.Lookup("wasm-memory-max-mb")
	if f == nil {
		t.Fatal("--wasm-memory-max-mb is not declared")
	}
	mb, err := strconv.Atoi(f.DefValue)
	if err != nil {
		t.Fatalf("--wasm-memory-max-mb default %q is not an integer: %v", f.DefValue, err)
	}
	if got, want := wasmtimeMemoryLimitBytes(mb), int64(256<<20); got != want {
		t.Fatalf("a stock worker hands the backend a %d byte memory limit, want %d (256 MiB, cleat#4171)", got, want)
	}
	if got := wasmtimeMemoryLimitBytes(mb); got != engine.DefaultWasmtimeMemoryLimitBytes {
		t.Fatalf("the stock limit %d is not the engine's default %d: the flag and the engine disagree", got, engine.DefaultWasmtimeMemoryLimitBytes)
	}
}

func TestAMemoryLimitTheOperatorSetsIsHonouredAndZeroIsTheEnginesDefault(t *testing.T) {
	for _, c := range []struct {
		mb   int
		want int64
	}{
		{32, 32 << 20},
		{64, 64 << 20},
		{512, 512 << 20},
		{0, engine.DefaultWasmtimeMemoryLimitBytes},
		{-1, engine.DefaultWasmtimeMemoryLimitBytes},
	} {
		if got := wasmtimeMemoryLimitBytes(c.mb); got != c.want {
			t.Errorf("wasmtimeMemoryLimitBytes(%d) = %d, want %d", c.mb, got, c.want)
		}
	}
}

func TestAStockWorkerRunsWithA1MiBOutputBuffer(t *testing.T) {
	f := flag.CommandLine.Lookup("wasm-output-buffer-size")
	if f == nil {
		t.Fatal("--wasm-output-buffer-size is not declared")
	}
	n, err := strconv.Atoi(f.DefValue)
	if err != nil {
		t.Fatalf("--wasm-output-buffer-size default %q is not an integer: %v", f.DefValue, err)
	}
	if n != 1<<20 {
		t.Fatalf("a stock worker runs with a %d byte output buffer, want %d (1 MiB, cleat#4171)", n, 1<<20)
	}
	if n != engine.DefaultOutBufSize {
		t.Fatalf("the stock buffer %d is not the engine's default %d: the flag and the engine disagree", n, engine.DefaultOutBufSize)
	}
}
