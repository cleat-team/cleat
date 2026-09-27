//go:build cgo

package engine

import (
	"github.com/bytecodealliance/wasmtime-go/v48"
)

// registerWasiPolicy re-binds every refused WASI function the module imports to
// a trap. cleat#1381.
//
// IT USES THE MODULE'S OWN DECLARED IMPORT TYPE, and that is not a stylistic
// choice. registerEnvStubs in this package records the measured cost of the
// alternative: a linker holds one definition per (module, name), so registering
// a FIXED shape satisfied one toolchain and locked out the other --
// AssemblyScript's four-argument abort against componentize-py's no-argument
// one, which failed at instantiation with "incompatible import type" before any
// policy could be consulted. Matching what the guest declares serves every
// toolchain from one linker and cannot turn a trap-on-call into a
// failure-to-link.
//
// A NIL MODULE APPLIES NO POLICY, which is safe only because the sole
// production caller passes a real one (backend_wasmtime.go). Tests that pass
// nil get the stock surface; TestTheWasmtimeBackendRefusesARefusedWasiCall is
// the end-to-end check that the production path does not.
func (b *wasmtimeBackend) registerWasiPolicy(linker *wasmtime.Linker, module *wasmtime.Module) error {
	if module == nil {
		return nil
	}
	// AFTER DefineWasi and after the determinism overrides, so this is the last
	// word on any name it touches. Bracketed the same way registerWasiDeterminism
	// is, so an accidental redefinition anywhere else still fails loudly.
	linker.AllowShadowing(true)
	defer linker.AllowShadowing(false)

	for _, imp := range module.Imports() {
		if imp.Module() != "wasi_snapshot_preview1" || imp.Name() == nil {
			continue
		}
		name := *imp.Name()
		if !wasiIsFatal(name) {
			continue
		}
		ft := imp.Type().FuncType()
		if ft == nil {
			continue
		}
		msg := wasiRefusalMessage(name)
		if err := linker.FuncNew("wasi_snapshot_preview1", name, ft,
			func(_ *wasmtime.Caller, _ []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				return nil, wasmtime.NewTrap(msg)
			}); err != nil {
			return err
		}
	}
	return nil
}
