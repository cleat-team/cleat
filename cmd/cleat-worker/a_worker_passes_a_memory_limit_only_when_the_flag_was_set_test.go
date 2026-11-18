package main

import (
	"flag"
	"testing"
)

// cleat#4077. The memory floor for a language whose runtime needs more than the 32 MiB default (C#: 64 MiB) applies only
// when the backend is told the operator chose no limit. The worker used to pass the flag's own default, 32, as if it were
// a choice, which pinned every C# guest on a real worker to 32 MiB; they die at startup with ".NET runtime failed to
// initialize". Nothing exercised that gate: the engine's tests build the backend directly, and reverting the condition to
// "always pass the flag's value" left every test green (cleat-review, #4082).
//
// Each case parses real arguments into a FlagSet declared exactly as config.go declares the flag, so what is asserted is
// the difference between "not passed" and "passed with the same value as the default", which is the one a value-only check
// cannot see.
func TestAWorkerPassesAMemoryLimitOnlyWhenTheFlagWasSet(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want int64
	}{
		{"not passed: the backend applies its default, per language", nil, 0},
		{"passed with the default's own value: a decision, honoured", []string{"-wasm-memory-max-mb=32"}, 32 << 20},
		{"passed with a larger value", []string{"-wasm-memory-max-mb=512"}, 512 << 20},
		{"passed as 0: documented as unset", []string{"-wasm-memory-max-mb=0"}, 0},
		{"another flag passed, this one not", []string{"-wasm-instance-timeout=5s"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			mb := fs.Int("wasm-memory-max-mb", 32, "")
			fs.Duration("wasm-instance-timeout", 0, "")
			if err := fs.Parse(c.args); err != nil {
				t.Fatalf("parse %v: %v", c.args, err)
			}
			if got := wasmtimeMemoryLimitFromFlags(fs, *mb); got != c.want {
				t.Fatalf("args %v: limit = %d, want %d", c.args, got, c.want)
			}
		})
	}

	// The flag as config.go really declares it must still default to 32, or "not passed" no longer means what these cases say.
	if f := flag.CommandLine.Lookup("wasm-memory-max-mb"); f == nil || f.DefValue != "32" {
		t.Fatalf("--wasm-memory-max-mb is not declared with default 32 (got %+v): the cases above model that default", f)
	}
}
