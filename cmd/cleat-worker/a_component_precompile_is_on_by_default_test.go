package main

import (
	"flag"
	"io"
	"log/slog"
	"testing"
)

// --component-precompile is ON by default since 0.6.0 (owner decision 2026-11-23, after the 2 GiB measurements: compiling in the worker retained up to about 300 MiB (198 to 308 for each of the first four further components, then 44 to 76) per distinct Python component and
// killed the container on the ninth, where the child completed). A default is the thing a flag cannot show you in a test that sets it, so this reads the registered default and the option the stock flags
// build, and the negative control turns it off and checks the same function builds nothing.
func TestComponentPrecompileIsOnByDefaultAndTurnsOff(t *testing.T) {
	f := flag.Lookup("component-precompile")
	if f == nil {
		t.Fatal("the --component-precompile flag is not registered")
	}
	if f.DefValue != "true" {
		t.Fatalf("--component-precompile defaults to %q, want \"true\"", f.DefValue)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := len(componentPrecompileOptionFromFlags(logger)); got != 1 {
		t.Fatalf("the stock flags built %d precompile options, want 1: the default is on in the flag and not in what the worker builds", got)
	}

	// The negative control: --component-precompile=false builds none, or the check above would pass for a function that always returns an option.
	old := *componentPrecompile
	*componentPrecompile = false
	defer func() { *componentPrecompile = old }()
	if got := len(componentPrecompileOptionFromFlags(logger)); got != 0 {
		t.Errorf("--component-precompile=false still built %d precompile options", got)
	}
}
