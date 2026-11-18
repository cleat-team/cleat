package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// A .NET guest cannot start under the engine's 32 MiB default: measured on a 704 KB hello-world, 52 MiB is the least it
// runs in, and below that it dies at startup with ".NET runtime failed to initialize" -- a message that names neither
// the limit nor the cause (cleat#4077). languageMemoryFloorBytes raises the default for that language only.
//
// The fixture is a 606-byte component whose core module declares 704 pages (44 MiB) of initial memory, so it needs
// more than the default at INSTANTIATION and nothing else; build.sh in testdata/wasi_minor regenerates it. What is
// asserted is the effect on the real backend:
//
//   - unstamped (detected as python), default limits: it must NOT run. This is the control -- without it, "it ran
//     under csharp" says nothing about the limit, because it may have run under any limit.
//   - stamped csharp, default limits: it runs.
//   - stamped csharp with an EXPLICIT 32 MiB limit: it must NOT run. The operator's number wins, even when it is
//     too small for the guest; the floor raises a default and never overrides a decision.

func bigmemComponent(t *testing.T, language string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "wasi_minor", "bigmem.wasm"))
	if err != nil {
		t.Fatalf("read bigmem.wasm: %v (build.sh regenerates it)", err)
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

func TestALanguageWithALargerRuntimeGetsALargerDefaultMemoryLimit(t *testing.T) {
	t.Run("control: an unstamped component is held to the 32 MiB default", func(t *testing.T) {
		if res, err := runBigmem(t, bigmemComponent(t, "")); err == nil {
			t.Fatalf("PRECONDITION FAILED: a component needing 44 MiB ran under the default limit (result %q), so this fixture "+
				"does not exceed the default and the csharp case below proves nothing.", res)
		}
	})
	t.Run("csharp runs under default limits", func(t *testing.T) {
		res, err := runBigmem(t, bigmemComponent(t, "csharp"))
		if err != nil {
			t.Fatalf("a csharp-stamped 44 MiB component failed under default limits: %v\n\nlanguageMemoryFloorBytes is "+
				"not reaching the component path's store. cleat#4077.", err)
		}
		if res != `{"ok":1}` {
			t.Fatalf("result %q, want {\"ok\":1}", res)
		}
	})
	t.Run("an explicit limit wins even when too small", func(t *testing.T) {
		if res, err := runBigmem(t, bigmemComponent(t, "csharp"), WithWasmtimeMemoryLimits(32<<20, 0, 0)); err == nil {
			t.Fatalf("an operator-set 32 MiB limit was overridden by the language floor (result %q): the floor must raise "+
				"a default, never an explicit decision.", res)
		}
	})
}

func TestMemoryLimitFor(t *testing.T) {
	const def = DefaultWasmtimeMemoryLimitBytes
	for _, c := range []struct {
		name string
		lim  wasmtimeLimits
		lang string
		want int64
	}{
		{"default, no floor language", wasmtimeLimits{memoryLimitBytes: def}, "python", def},
		{"default, csharp is raised", wasmtimeLimits{memoryLimitBytes: def}, "csharp", languageMemoryFloorBytes["csharp"]},
		{"explicit small beats the floor", wasmtimeLimits{memoryLimitBytes: 32 << 20, memoryLimitExplicit: true}, "csharp", 32 << 20},
		{"explicit large is kept", wasmtimeLimits{memoryLimitBytes: 512 << 20, memoryLimitExplicit: true}, "csharp", 512 << 20},
		{"a default already above the floor is not lowered", wasmtimeLimits{memoryLimitBytes: 128 << 20}, "csharp", 128 << 20},
	} {
		if got := c.lim.memoryLimitFor(c.lang); got != c.want {
			t.Errorf("%s: memoryLimitFor(%q) = %d, want %d", c.name, c.lang, got, c.want)
		}
	}
	if floor := languageMemoryFloorBytes["csharp"]; floor < 52<<20 {
		t.Errorf("the csharp floor is %d bytes, below the measured 52 MiB minimum", floor)
	}
}
