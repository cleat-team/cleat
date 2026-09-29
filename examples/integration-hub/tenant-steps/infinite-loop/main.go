// infinite-loop is NOT a tenant's real workflow. It is the wedge scenario's
// third adversarial probe, standing in for a malicious or merely careless
// tenant upload that never returns -- a runaway script, not one that tries
// to escape the sandbox through a host call.
//
// That distinction is the point of this fixture existing alongside
// malicious-read-host-file rather than as a variant of it: the read-host-file
// probe is stopped by a HOST CALL policy (engine/wasi_policy.go traps
// path_open before the guest's own code can act on the result), which says
// nothing about a guest that never calls into the host at all. A tight loop
// with no syscalls, no service calls, no plugin calls -- nothing for a
// per-call policy to intercept -- is a different failure mode, and cleat's
// answer to it is a different mechanism: the wasmtime epoch fence
// (engine.WithWASMInstanceTimeout / engine/backend_wasmtime.go), which
// interrupts guest execution on a clock tick regardless of what the guest is
// doing. See docs/playbooks/integration-hub.md's wedge claims and
// engine/tenant_instance_timeout_test.go for the same mechanism proven at
// the engine layer; this fixture and the scenario script that runs it prove
// it through the real deployed path -- an uploaded WASM module, a running
// worker, an HTTP status poll -- rather than a direct call into the backend.
//
// A BUSY LOOP, NOT time.Sleep OR A BLOCKING SYSCALL. Either of those would
// yield to the scheduler or the host, which is a different (and already
// covered) code path: a host call blocked on something slow is what
// --wasm-wall-clock-ceiling bounds, not --wasm-instance-timeout. This loop
// does no I/O and calls into no host function at all, so the epoch fence is
// the ONLY thing that can ever stop it.
package infiniteloop

import "github.com/cleat-team/cleat/cleat"

func InfiniteLoop(h cleat.HostCalls, input string) (string, error) {
	//nolint:staticcheck // SA5002: the spin IS the fixture -- this function
	// exists to be a busy loop the epoch fence must interrupt (cleat#2628).
	for {
	}
}
