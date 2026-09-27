package engine

import (
	"context"
	"strings"
	"testing"
)

// wasmStartTraps returns a module exporting "_start" as () -> () whose body is
// a single `unreachable`, plus one page of memory.
//
// The memory matters: InitModule's readiness probe returns as soon as
// mod.Memory() is live, so without it the function would take the
// no-memory path and never reach the assertion this test is about.
func wasmStartTraps() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
		// Type section: 1 type, () -> ()
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		// Function section: 1 function, type 0
		0x03, 0x02, 0x01, 0x00,
		// Memory section: 1 memory, min=1 page, max=1 page
		0x05, 0x04, 0x01, 0x01, 0x01, 0x01,
		// Export section: "_start" -> func 0, "memory" -> mem 0
		0x07, 0x13, 0x02, // section len 19 = 1 count + 9 + 9
		0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00, // "_start" func 0
		0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00, // "memory" mem 0
		// Code section: body = unreachable + end
		0x0a, 0x05, 0x01, 0x03, 0x00, 0x00, 0x0b,
	}
}

// TestInitModuleReportsATrappingStart covers the error InitModule used to throw
// away.
//
// It dispatches _start in a goroutine and reads the result through errCh, which
// is consumed in three places. But errCh was only ever WRITTEN on panic:
//
//	go func() {
//	    defer func() {
//	        if r := recover(); r != nil { errCh <- ... }
//	        close(done)
//	    }()
//	    start.Call(ctx)          // returned error discarded
//	}()
//
// wazero signals a trap by returning an error, not by panicking, so the common
// failure was discarded while the rare one was caught. `close(done)` still
// fired, InitModule returned nil, and a guest whose _start had trapped was
// reported as successfully initialised -- with the damage surfacing later, on
// some unrelated export call.
//
// The machinery to report it was already there and simply not connected: this
// is the same shape as several other defects found this week, a signal that
// existed but was attached to the wrong thing.
func TestInitModuleReportsATrappingStart(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, wasmStartTraps())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	err = rt.InitModule(ctx, mod)
	if err == nil {
		t.Fatal("InitModule returned nil for a module whose _start traps.\n\n" +
			"The caller now believes the guest initialised, so the failure resurfaces " +
			"later on an unrelated export call rather than here, where it happened.")
	}
	if !strings.Contains(err.Error(), "_start failed") {
		t.Errorf("InitModule error = %v, want one naming _start so the reader knows "+
			"which phase failed", err)
	}
}

// TestInitModuleAcceptsAModuleWithoutStart is one half of the control: a module
// with no _start export is a legitimate no-op (Rust and C guests), not a
// failure. Without it, the assertion above would pass against an InitModule
// that rejected everything.
func TestInitModuleAcceptsAModuleWithoutStart(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// wasmWithUnreachable exports "run", not "_start".
	compiled, err := rt.CompileModule(ctx, wasmWithUnreachable())
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
		t.Errorf("InitModule on a module with no _start export: %v\n\n"+
			"That is the Rust/C guest shape and must stay a no-op.", err)
	}
}

// wasmStartExitsZero returns a module whose "_start" calls
// wasi_snapshot_preview1.proc_exit(0) -- the way every Go wasip1 guest
// terminates. wazero reports that as *sys.ExitError with code 0, so this is
// the path InitModule must treat as success rather than failure.
func wasmStartExitsZero() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
		// Type section: 2 types -- (i32)->() for proc_exit, ()->() for _start
		0x01, 0x08, 0x02,
		0x60, 0x01, 0x7f, 0x00, // type 0: (i32) -> ()
		0x60, 0x00, 0x00, // type 1: () -> ()
		// Import section: wasi_snapshot_preview1.proc_exit, type 0
		0x02, 0x24, 0x01, // section len 36 = 1 count + 23 module + 10 field + 2
		0x16, 0x77, 0x61, 0x73, 0x69, 0x5f, 0x73, 0x6e, 0x61, 0x70, 0x73,
		0x68, 0x6f, 0x74, 0x5f, 0x70, 0x72, 0x65, 0x76, 0x69, 0x65, 0x77, 0x31,
		0x09, 0x70, 0x72, 0x6f, 0x63, 0x5f, 0x65, 0x78, 0x69, 0x74,
		0x00, 0x00,
		// Function section: 1 function, type 1
		0x03, 0x02, 0x01, 0x01,
		// Memory section: 1 memory, min=1 page, max=1 page
		0x05, 0x04, 0x01, 0x01, 0x01, 0x01,
		// Export section: "_start" -> func 1 (import occupies index 0)
		0x07, 0x13, 0x02,
		0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x01,
		0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
		// Code section: i32.const 0; call 0; end
		0x0a, 0x08, 0x01, 0x06, 0x00, 0x41, 0x00, 0x10, 0x00, 0x0b,
	}
}

// TestInitModuleTreatsExitZeroAsSuccess is the control that the trap test needs
// and did not have.
//
// A Go wasip1 _start runs main() and terminates via proc_exit, which wazero
// surfaces as an error (*sys.ExitError, code 0). Reporting every error from
// start.Call would therefore break EVERY Go guest, so InitModule carves out
// exit code 0.
//
// That carve-out was unverified when written: inverting it
// (`ExitCode() == 0` -> `== 999`) left the entire engine suite green, which
// means nothing exercised a real proc_exit(0). This test closes that gap, so a
// future change to the classification cannot pass silently.
func TestInitModuleTreatsExitZeroAsSuccess(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, wasmStartExitsZero())
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
		t.Errorf("InitModule reported a failure for a guest whose _start exited 0: %v\n\n"+
			"That is how every Go wasip1 guest terminates, so this would break all of "+
			"them. exit(0) is a normal completion, not a trap.", err)
	}
}
