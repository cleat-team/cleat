package main

import (
	"flag"
	"strconv"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4130: the worker's --wasm-max-string-len and the engine's DefaultMaxWasmStringLen were two different numbers (64 KiB and 1 MiB), and the
// flag's was the one a running worker used, so a document that quoted the engine's figure described a limit nothing enforced. The flag now takes
// its default from the engine's constant; this pins that, and the value the owner chose (2 MiB).
func TestTheWorkersStringLimitFlagDefaultsToTheEngines(t *testing.T) {
	f := flag.Lookup("wasm-max-string-len")
	if f == nil {
		t.Fatal("the worker declares no --wasm-max-string-len flag")
	}
	got, err := strconv.Atoi(f.DefValue)
	if err != nil {
		t.Fatalf("the flag's default %q is not a number: %v", f.DefValue, err)
	}
	if got != engine.DefaultMaxWasmStringLen {
		t.Errorf("--wasm-max-string-len defaults to %d, the engine's DefaultMaxWasmStringLen is %d: a running worker would enforce one and the docs quote the other", got, engine.DefaultMaxWasmStringLen)
	}
	if engine.DefaultMaxWasmStringLen != 2<<20 {
		t.Errorf("DefaultMaxWasmStringLen = %d, want 2 MiB (2097152), the limit chosen in cleat#4130", engine.DefaultMaxWasmStringLen)
	}
	if engine.MaxWasmStringLen != engine.DefaultMaxWasmStringLen {
		t.Errorf("engine.MaxWasmStringLen starts at %d, want the default %d", engine.MaxWasmStringLen, engine.DefaultMaxWasmStringLen)
	}
}
