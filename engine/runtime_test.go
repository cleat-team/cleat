package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/sys"
)

// ---- phase A: wasmTrapError tests ----

func TestWasmTrapError_Error(t *testing.T) {
	e := &wasmTrapError{msg: "wasm trap: unreachable"}
	if got := e.Error(); got != "wasm trap: unreachable" {
		t.Errorf("Error() = %q, want %q", got, "wasm trap: unreachable")
	}
}

func TestWasmTrapError_Unwrap(t *testing.T) {
	cause := errors.New("underlying cause")
	e := &wasmTrapError{cause: cause, msg: "wasm trap: something"}
	if !errors.Is(e, cause) {
		t.Error("errors.Is(e, cause) = false, want true")
	}
	if got := e.Unwrap(); got != cause {
		t.Errorf("Unwrap() = %v, want %v", got, cause)
	}
}

// ---- phase A: formatWasmCallError tests ----

func TestFormatWasmCallError(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantContain string
	}{
		{
			name:        "context canceled",
			err:         sys.NewExitError(sys.ExitCodeContextCanceled),
			wantContain: "wasm trap: context canceled",
		},
		{
			name:        "deadline exceeded",
			err:         sys.NewExitError(sys.ExitCodeDeadlineExceeded),
			wantContain: "wasm trap: deadline exceeded",
		},
		{
			name:        "exit code",
			err:         sys.NewExitError(42),
			wantContain: "wasm trap: exit(code=42)",
		},
		{
			name:        "wasm error prefix replacement",
			err:         errors.New("wasm error: unreachable\nwasm stack trace:\n    .my_func\n        0x1234: /build/workflow.go:42:5"),
			wantContain: "wasm trap: unreachable\nwasm stack trace:",
		},
		{
			name:        "fallback plain error",
			err:         errors.New("something broke"),
			wantContain: "wasm trap: something broke",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatWasmCallError(tt.err)
			if got == nil {
				t.Fatal("formatWasmCallError returned nil")
			}
			msg := got.Error()
			if !strings.Contains(msg, tt.wantContain) {
				t.Errorf("Error() = %q, want containing %q", msg, tt.wantContain)
			}
			// All results should be *wasmTrapError
			wt, ok := got.(*wasmTrapError)
			if !ok {
				t.Fatalf("expected *wasmTrapError, got %T", got)
			}
			// Unwrap should return the original error (or at least wrap it)
			if !errors.Is(got, tt.err) {
				t.Errorf("errors.Is(result, original) = false, want true (got: %v, original: %v)", wt.Unwrap(), tt.err)
			}
		})
	}
}

// ---- phase A: fuelMeter tests ----

func TestFuelMeter_NewFunctionListener(t *testing.T) {
	fm := &fuelMeter{}
	got := fm.NewFunctionListener(nil)
	if got != fm {
		t.Errorf("NewFunctionListener should return self, got %v", got)
	}
	if _, ok := got.(*fuelMeter); !ok {
		t.Errorf("NewFunctionListener returned %T, want *fuelMeter", got)
	}
}

func TestFuelMeter_Before(t *testing.T) {
	ctx := context.Background()

	t.Run("fuel not exhausted", func(t *testing.T) {
		rt, err := NewRuntime(ctx, 0, 0)
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		defer rt.Close(ctx)

		compiled, err := rt.CompileModule(ctx, minimalWasm())
		if err != nil {
			t.Fatalf("CompileModule: %v", err)
		}
		defer compiled.Close(ctx)

		mod, err := rt.InstantiateModule(ctx, compiled)
		if err != nil {
			t.Fatalf("InstantiateModule: %v", err)
		}
		defer mod.Close(ctx)

		fm := &fuelMeter{}
		fm.remaining.Store(10) // plenty of fuel

		fm.Before(ctx, mod, nil, nil, nil)
		if mod.IsClosed() {
			t.Error("module should not be closed when fuel remains")
		}
	})

	t.Run("fuel exhausted", func(t *testing.T) {
		rt, err := NewRuntime(ctx, 0, 0)
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		defer rt.Close(ctx)

		compiled, err := rt.CompileModule(ctx, minimalWasm())
		if err != nil {
			t.Fatalf("CompileModule: %v", err)
		}
		defer compiled.Close(ctx)

		mod, err := rt.InstantiateModule(ctx, compiled)
		if err != nil {
			t.Fatalf("InstantiateModule: %v", err)
		}

		fm := &fuelMeter{}
		fm.remaining.Store(1) // exactly 1, will decrement to 0

		fm.Before(ctx, mod, nil, nil, nil)
		if !mod.IsClosed() {
			t.Error("module should be closed when fuel is exhausted")
		}
	})
}

func TestFuelMeter_After(t *testing.T) {
	fm := &fuelMeter{}
	// Should not panic with nil params.
	fm.After(nil, nil, nil, nil)
}

func TestFuelMeter_Abort(t *testing.T) {
	fm := &fuelMeter{}
	// Should not panic with nil params.
	fm.Abort(nil, nil, nil, nil)
}

// ---- phase B: InitModule tests ----

// wasmWithStart builds a minimal WASM module with a no-op _start export
// and a memory section (required for InitModule's liveness check).
// WASM binary:
//
//	module (memory 1) (func (export "_start") (type 0) end)
func wasmWithStart() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
		// type section: 1 func type [] -> []
		0x01,       // section id
		0x04,       // section size (count + type = 1 + 3)
		0x01,       // count
		0x60,       // func
		0x00, 0x00, // 0 params, 0 results
		// function section: 1 function using type 0
		0x03, // section id
		0x02, // section size
		0x01, // count
		0x00, // type index
		// memory section: 1 memory, 1 page initial (no max)
		0x05, // section id
		0x03, // section size
		0x01, // count
		0x00, // flags (no max)
		0x01, // initial pages (64KB)
		// export section: "_start" = func 0
		0x07,                                           // section id
		0x0a,                                           // section size (count + name_len + name + kind + idx = 1 + 1 + 6 + 1 + 1 = 10)
		0x01,                                           // count
		0x06,                                           // name length
		0x5f, 0x73, 0x74, 0x61, 0x72, 0x74,             // "_start"
		0x00,                                           // kind (func)
		0x00,                                           // index
		// code section: 1 function body (locals=0, end)
		0x0a, // section id
		0x04, // section size (count + body_size + locals + end = 1 + 1 + 1 + 1 = 4)
		0x01, // count
		0x02, // body size
		0x00, // locals count
		0x0b, // end
	}
}

func TestInitModule_NoStart(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	if err := rt.InitModule(ctx, mod); err != nil {
		t.Errorf("InitModule with no _start: %v", err)
	}
}

func TestInitModule_WithStart(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, wasmWithStart())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	if err := rt.InitModule(ctx, mod); err != nil {
		t.Errorf("InitModule with _start: %v", err)
	}
}
