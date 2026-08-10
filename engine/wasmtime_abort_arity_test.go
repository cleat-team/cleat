//go:build cgo

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// abortModuleWAT builds a module importing env.abort with the given parameter
// list, so the two toolchain shapes can be exercised without either a
// componentize-py binary or the 19 MB checked-in Python fixture.
func abortModuleWAT(t *testing.T, params string) []byte {
	t.Helper()
	wasmBytes, err := wasmtime.Wat2Wasm(`(module
  (import "env" "abort" (func $abort ` + params + `))
  (memory (export "memory") 1)
  (func (export "main"))
)`)
	if err != nil {
		t.Fatalf("wat2wasm: %v", err)
	}
	return wasmBytes
}

// TestEnvAbortArityMatchesTheModule is the regression test for a linker that
// could serve one toolchain's abort or the other's, but not both.
//
// registerEnvStubs registered env.abort unconditionally as
// (msg, file, line, col i32) -- AssemblyScript's shape. A module importing a
// no-argument abort, which is what the core modules inside a componentize-py
// component do, was rejected at instantiation:
//
//	incompatible import type for `env::abort`
//	expected type `(func)`, found type `(func (param i32 i32 i32 i32))`
//
// The comment on that registration argued the mismatch was benign because
// DefineUnknownImportsAsTraps would cover the other signature and "the first
// registration wins". The first registration does win -- that is the defect.
// Instantiation fails before any trap-default can apply.
func TestEnvAbortArityMatchesTheModule(t *testing.T) {
	cases := []struct {
		name   string
		params string
	}{
		{"assemblyscript shape", "(param i32 i32 i32 i32)"},
		{"no-argument shape", ""},
		{"single argument", "(param i32)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, err := NewWasmtimeBackend(ctx)
			if err != nil {
				t.Fatalf("wasmtime backend failed to initialise: %v", err)
			}
			defer b.Close(ctx)

			module, err := wasmtime.NewModule(b.engine, abortModuleWAT(t, tc.params))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			// Reproduce the instantiation path's linker setup rather than
			// calling Execute: this is about whether the import resolves, and
			// Execute would go on to need a host session it does not have here.
			linker := wasmtime.NewLinker(b.engine)
			var cr, ce string
			if err := b.registerAllImports(linker, &cr, &ce, false, abortImportType(module)); err != nil {
				t.Fatalf("registerAllImports: %v", err)
			}

			store := wasmtime.NewStore(b.engine)
			defer store.Close()
			if _, err := linker.Instantiate(store, module); err != nil {
				if strings.Contains(err.Error(), "env::abort") ||
					strings.Contains(err.Error(), "incompatible import type") {
					t.Fatalf("env.abort did not resolve for %s: %v", tc.name, err)
				}
				t.Fatalf("instantiate: %v", err)
			}
		})
	}
}

// TestAbortImportTypeReadsTheDeclaredType covers the lookup on its own,
// including the case that decides the fallback: a module importing no abort at
// all must yield nil so registerEnvStubs keeps the historical shape rather than
// registering something arbitrary.
func TestAbortImportTypeReadsTheDeclaredType(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("wasmtime backend failed to initialise: %v", err)
	}
	defer b.Close(ctx)

	t.Run("nil module", func(t *testing.T) {
		if got := abortImportType(nil); got != nil {
			t.Errorf("abortImportType(nil) = %v, want nil", got)
		}
	})

	t.Run("no abort import", func(t *testing.T) {
		plain, werr := wasmtime.Wat2Wasm(`(module (func (export "main")))`)
		if werr != nil {
			t.Fatalf("wat2wasm: %v", werr)
		}
		m, err := wasmtime.NewModule(b.engine, plain)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if got := abortImportType(m); got != nil {
			t.Errorf("abortImportType = %v, want nil for a module with no abort import", got)
		}
	})

	t.Run("reads the arity", func(t *testing.T) {
		for _, tc := range []struct {
			params string
			want   int
		}{
			{"(param i32 i32 i32 i32)", 4},
			{"", 0},
			{"(param i32 i64)", 2},
		} {
			m, err := wasmtime.NewModule(b.engine, abortModuleWAT(t, tc.params))
			if err != nil {
				t.Fatalf("compile %q: %v", tc.params, err)
			}
			ft := abortImportType(m)
			if ft == nil {
				t.Fatalf("abortImportType returned nil for %q", tc.params)
			}
			if got := len(ft.Params()); got != tc.want {
				t.Errorf("params for %q = %d, want %d", tc.params, got, tc.want)
			}
		}
	})
}
