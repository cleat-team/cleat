package main

import (
	"flag"
	"strconv"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4132: the response ceiling is its own flag, defaulting to the engine's constant (2 MiB), and it is NOT --wasm-output-buffer-size, which
// sizes the entry-point regions in linear memory.
func TestTheWorkersResponseLimitFlagDefaultsToTheEnginesAndIsNotTheOutputBuffer(t *testing.T) {
	f := flag.Lookup("wasm-max-response-bytes")
	if f == nil {
		t.Fatal("the worker declares no --wasm-max-response-bytes flag")
	}
	got, err := strconv.Atoi(f.DefValue)
	if err != nil || got != engine.DefaultMaxHostCallResponseBytes {
		t.Errorf("--wasm-max-response-bytes defaults to %q, want the engine's DefaultMaxHostCallResponseBytes (%d)", f.DefValue, engine.DefaultMaxHostCallResponseBytes)
	}
	if engine.DefaultMaxHostCallResponseBytes != 2<<20 {
		t.Errorf("DefaultMaxHostCallResponseBytes = %d, want 2 MiB", engine.DefaultMaxHostCallResponseBytes)
	}
	if ob := flag.Lookup("wasm-output-buffer-size"); ob == nil || ob.DefValue == f.DefValue {
		t.Errorf("--wasm-output-buffer-size must stay a separate setting with its own default (found %v)", ob)
	}
}
