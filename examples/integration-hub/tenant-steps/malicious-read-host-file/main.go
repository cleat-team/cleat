// malicious-read-host-file is NOT a tenant's real workflow. It is the wedge
// scenario's own adversarial probe, standing in for a malicious or merely
// careless tenant upload, and it does the simplest thing such an upload could
// do to escape cleat's sandbox: read a file from the HOST filesystem cleat
// never gave it.
//
// Under Go's wasip1 target this reaches the WASI path_open call, and
// engine/wasi_policy.go lists path_open as wasiFatal -- "filesystem access;
// the family this policy most exists to refuse". The refusal is a hard TRAP
// (engine/wasi_policy_wasmtime.go), not an ordinary error this function's own
// `if err != nil` can catch: a trap aborts the whole guest instance before
// control returns here at all. So this function's error-returning arm exists
// for the DIFFERENT question worth distinguishing -- "the read failed without
// escaping the sandbox" is not the same claim as "the sandbox refused it" --
// and the scenario that runs this workflow asserts on the ACTUAL failure text
// it gets back, rather than assuming which arm produced it.
//
// docs/playbooks/integration-hub.md's wedge claim: "no ambient filesystem".
// This is that claim, executed against a real deployed worker.
//
// IT USES syscall, NOT os, AND THAT IS THE POINT. `cleat build`'s static
// analyzer (internal/closure, error E010) refuses any Go guest that imports
// "os" -- confirmed by trying: `cleat build` exits 1 and writes no WASM at
// all for a version of this file that called os.ReadFile. That check is
// keyed on the "os" import path alone, so it is a convenience that catches
// the common case, not the security boundary; a tenant's own toolchain has
// no reason to go through cleat's Go-specific analyzer at all, and a
// hand-rolled build, a different language SDK, or -- as here -- one
// unchecked package that still reaches the same WASI import proves the
// guarantee has to live where this package's build actually landed: the
// HOST, in engine/wasi_policy.go, which traps path_open regardless of which
// Go package a guest used to reach it.
package maliciousreadhostfile

import (
	"syscall"

	"github.com/cleat-team/cleat/cleat"
)

func ReadHostFile(h cleat.HostCalls, input string) (string, error) {
	fd, err := syscall.Open("/etc/hostname", syscall.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)
	buf := make([]byte, 256)
	n, err := syscall.Read(fd, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
