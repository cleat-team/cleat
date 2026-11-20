package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// A .NET guest cannot start under 52 MiB: measured on a 704 KB hello-world, that is the least it runs in, and below it the guest dies at startup
// with ".NET runtime failed to initialize" -- a message that names neither the limit nor the cause (cleat#4077). Until cleat#4171 the default limit
// was 32 MiB and a per-language floor raised it for C#. The default is now 256 MiB (owner decision, 2026-11-19), which clears every runtime's
// minimum, so the floor could never fire and was removed; THIS test is what stops the default being lowered below that minimum again.
//
// The fixture is a 606-byte component whose core module declares 704 pages (44 MiB) of initial memory, so it needs more than 32 MiB at
// INSTANTIATION and nothing else; build.sh in testdata/wasi_minor regenerates it. What is asserted is the effect on the real backend:
//
//   - an EXPLICIT 32 MiB limit refuses it. This is the control: without it, "it ran under the default" says nothing about the limit, because
//     the fixture might fit anywhere. It also pins that an operator's number wins, even when it is too small for the guest.
//   - the DEFAULT limit runs it, for an unstamped component (detected as python) and for one stamped csharp.

// dotnetMinimumMemoryBytes is the least linear memory a .NET (NativeAOT-LLVM) guest starts in, measured on a hello-world (cleat#4077).
const dotnetMinimumMemoryBytes = 52 << 20

func bigmemComponent(t *testing.T, language string) []byte {
	t.Helper()
	return bigmemFixture(t, "bigmem.wasm", language)
}

// bigmemFixture reads one of the memory fixtures in testdata/wasi_minor (build.sh regenerates them), optionally stamped with a language.
func bigmemFixture(t *testing.T, file, language string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "wasi_minor", file))
	if err != nil {
		t.Fatalf("read %s: %v (build.sh regenerates it)", file, err)
	}
	if language == "" {
		return b
	}
	stamped, err := wasm.WriteMetadata(b, &wasm.Metadata{Language: language})
	if err != nil {
		t.Fatalf("stamp %q: %v", language, err)
	}
	if got := wasm.DetectLanguage(stamped); got != language {
		t.Fatalf("PRECONDITION FAILED: stamped %q but DetectLanguage says %q, so nothing below tests that language", language, got)
	}
	return stamped
}

func runBigmem(t *testing.T, wasmBytes []byte, opts ...WasmtimeOption) (string, error) {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, opts...)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	eng := NewEngine(rt, &mockCaller{}, WithBackends(langs, wt))
	res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "run", []byte(`{}`))
	return res, err
}

func TestTheDefaultMemoryLimitClearsTheLargestRuntime(t *testing.T) {
	for _, language := range []string{"", "csharp"} {
		name := language
		if name == "" {
			name = "unstamped (python)"
		}
		t.Run("control: an explicit 32 MiB limit refuses the 44 MiB component, "+name, func(t *testing.T) {
			if res, err := runBigmem(t, bigmemComponent(t, language), WithWasmtimeMemoryLimits(32<<20, 0, 0)); err == nil {
				t.Fatalf("PRECONDITION FAILED: a component needing 44 MiB ran under an explicit 32 MiB limit (result %q), so this fixture does "+
					"not exceed 32 MiB and the default-limit cases below prove nothing.", res)
			}
		})
		t.Run("the default limit runs it, "+name, func(t *testing.T) {
			res, err := runBigmem(t, bigmemComponent(t, language))
			if err != nil {
				t.Fatalf("a 44 MiB component failed under the DEFAULT limit: %v\n\nthe default has fallen below what a runtime needs to start. cleat#4077.", err)
			}
			if res != `{"ok":1}` {
				t.Fatalf("result %q, want {\"ok\":1}", res)
			}
		})
	}
}

// The numbers, pinned: the owner's decision (cleat#4171) is 256 MiB, and it has to stay above the measured .NET minimum.
func TestTheDefaultMemoryLimitIs256MiBAndAboveTheDotnetMinimum(t *testing.T) {
	if DefaultWasmtimeMemoryLimitBytes != 256<<20 {
		t.Errorf("DefaultWasmtimeMemoryLimitBytes = %d, want %d (256 MiB, owner decision cleat#4171); change this test with that decision, not around it", DefaultWasmtimeMemoryLimitBytes, int64(256<<20))
	}
	if DefaultWasmtimeMemoryLimitBytes < dotnetMinimumMemoryBytes {
		t.Errorf("the default %d bytes is below the measured .NET start-up minimum %d: a C# guest would die with \".NET runtime failed to initialize\" (cleat#4077)", DefaultWasmtimeMemoryLimitBytes, int64(dotnetMinimumMemoryBytes))
	}
}

// The mirror, and the half the tests above could not see. They prove the default is HIGH ENOUGH (a 44 MiB component runs under it) and that an
// explicit 32 MiB limit refuses; nothing showed the DEFAULT refuses anything. A backend constructor that left the limit at 0, so every stock store was
// unbounded, passed all of them (found in review of cleat#4200 by making exactly that change). bigmem-over-default.wasm needs 260 MiB at
// instantiation, more than the 256 MiB default.
//
//   - control: under an explicit 512 MiB limit it RUNS. Without this, "refused under the default" would also be true of a fixture that cannot run anywhere.
//   - the default refuses it, for an unstamped component and a csharp-stamped one (no language gets an unbounded store).
func TestTheDefaultMemoryLimitIsALimitNotJustAFloor(t *testing.T) {
	const over = "bigmem-over-default.wasm"
	t.Run("control: an explicit 512 MiB limit runs the 260 MiB component", func(t *testing.T) {
		res, err := runBigmem(t, bigmemFixture(t, over, ""), WithWasmtimeMemoryLimits(512<<20, 0, 0))
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: the 260 MiB component does not run even under a 512 MiB limit: %v, so a refusal under the default proves nothing", err)
		}
		if res != `{"ok":1}` {
			t.Fatalf("result %q, want {\"ok\":1}", res)
		}
	})
	for _, language := range []string{"", "csharp"} {
		name := language
		if name == "" {
			name = "unstamped (python)"
		}
		t.Run("the default limit refuses it, "+name, func(t *testing.T) {
			if res, err := runBigmem(t, bigmemFixture(t, over, language)); err == nil {
				t.Fatalf("a component needing 260 MiB RAN under the default limit (result %q): the stock memory limit is not being enforced, "+
					"so every stock store is unbounded. DefaultWasmtimeMemoryLimitBytes is %d.", res, DefaultWasmtimeMemoryLimitBytes)
			}
		})
	}
}
