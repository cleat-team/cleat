//go:build cgo

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// TestTheWasmtimeBackendRefusesARefusedWasiCall is the wasmtime half of
// cleat#1381's known-positive, and the worker's half: wasmtime is the only
// backend a worker runs, so a policy enforced on wazero alone would be a policy
// that never applies in production.
//
// It goes through registerAllImports -- the production registration path --
// rather than calling registerWasiPolicy directly, because the ORDER of the
// three WASI registrations is the thing most likely to be wrong: DefineWasi
// binds the stock surface, the determinism overrides replace two names, and
// this policy must be last. A direct call would pass with the order reversed.
func TestTheWasmtimeBackendRefusesARefusedWasiCall(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, name string, nparams int) error {
		t.Helper()
		wb, err := NewWasmtimeBackend(ctx)
		if err != nil {
			t.Fatalf("NewWasmtimeBackend: %v", err)
		}
		t.Cleanup(func() { wb.Close(ctx) })

		store := wasmtime.NewStore(wb.engine)
		t.Cleanup(func() { store.Close() })
		if _, err := wb.configureStore(ctx, store); err != nil {
			t.Fatalf("configureStore: %v", err)
		}
		wasiConfig := wasmtime.NewWasiConfig()
		wasiConfig.InheritStderr()
		store.SetWasi(wasiConfig)

		module, err := wasmtime.NewModule(wb.engine, minimalGuestCalling(name, nparams))
		if err != nil {
			t.Fatalf("NewModule(%s): %v", name, err)
		}
		t.Cleanup(func() { module.Close() })

		var completeResult, completeErr string
		linker := wasmtime.NewLinker(wb.engine)
		if err := wb.registerAllImports(linker, &completeResult, &completeErr, true, module); err != nil {
			t.Fatalf("registerAllImports: %v", err)
		}
		inst, err := linker.Instantiate(store, module)
		if err != nil {
			t.Fatalf("a guest importing %s did not even LINK: %v\n\n"+
				"The trap's signature must match what the guest DECLARES; it is taken "+
				"from the module's own import type for exactly this reason. A "+
				"link-time break is worse than a wrong bucket, because every Go guest "+
				"imports poll_oneoff without ever calling it.", name, err)
		}
		_, err = inst.GetFunc(store, "run").Call(store)
		return err
	}

	err := run(t, "fd_datasync", 1)
	if err == nil {
		t.Fatal("a guest CALLED fd_datasync on the wasmtime backend and it returned " +
			"normally. This is the backend a worker runs, so the allowlist is not in " +
			"force where it matters. Check that registerWasiPolicy runs AFTER " +
			"DefineWasi and inside its AllowShadowing bracket -- without shadowing, " +
			"the redefinition fails and DefineWasi's binding stands.")
	}
	if !strings.Contains(err.Error(), "cleat refuses the WASI call") {
		t.Fatalf("fd_datasync failed on wasmtime, but not with cleat's refusal:\n  %v\n\n"+
			"The refused families already failed for want of a preopened directory "+
			"before this policy existed. Reading that as enforcement would leave the "+
			"policy untested on the only backend a worker uses.", err)
	}

	// NEGATIVE CONTROL -- see the wazero test. An allowlist that refuses
	// everything passes every "is it refused?" assertion.
	if err := run(t, "sched_yield", 0); err != nil {
		t.Fatalf("sched_yield is on the allowlist and wasmtime refused it anyway: %v", err)
	}
	t.Log("wasmtime: fd_datasync refused, sched_yield allowed")
}
