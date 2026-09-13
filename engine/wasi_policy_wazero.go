package engine

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// wasiRefusalMessage is what a guest sees when it calls a refused WASI
// function. It names the function, the reason, and the cleat facility that
// replaces it, because the alternative -- a bare trap -- is indistinguishable
// from a guest bug and sends the reader into their own code.
func wasiRefusalMessage(name string) string {
	reason := "not on the WASI allowlist"
	if e, ok := wasiPreview1Policy[name]; ok {
		reason = e.reason
	}
	return fmt.Sprintf(
		"cleat refuses the WASI call %q: %s\n"+
			"This is cleat's WASI policy (cleat#1381), not a guest bug: a durable "+
			"workflow may reach only the functions on a stated allowlist, so that a "+
			"replay reproduces what the first execution did. See engine/wasi_policy.go.",
		name, reason)
}

// applyWasiPolicyWazero re-exports every refused function as a trap.
//
// ORDER IS LOAD BEARING AND SILENT IF WRONG. wazero's host module builder is
// LAST-WINS: a name registered after the stock exporter replaces it, and a name
// registered BEFORE is discarded without an error. Measured 2026-09-13 by
// re-exporting path_open both ways and reading the surviving definition's
// parameter TYPES -- the arity alone did not separate them, and the count of
// exported functions stayed 46 either way, so both orders look identical to
// every check except the one that asks whose signature survived.
//
// SIGNATURES ARE DERIVED FROM stock, NEVER TRANSCRIBED. A trap whose type
// disagrees with the real function turns a trap-on-call into a failure to LINK,
// and a guest that merely imports the name never loads. Every Go guest imports
// poll_oneoff, so a transcription slip there would break every Go workflow in
// the repository -- at instantiation, before any of this policy is consulted.
func applyWasiPolicyWazero(b wazero.HostModuleBuilder, stock wazero.CompiledModule) {
	for name, def := range stock.ExportedFunctions() {
		if !wasiIsFatal(name) {
			continue
		}
		name := name
		b.NewFunctionBuilder().WithGoModuleFunction(
			api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
				// A panic in a wazero host function surfaces as an error from
				// the guest call rather than unwinding the host, which is the
				// documented way for a host function to fail (api.GoModuleFunc).
				panic(wasiRefusalMessage(name))
			}),
			def.ParamTypes(), def.ResultTypes(),
		).Export(name)
	}
}
