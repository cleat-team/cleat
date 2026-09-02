// Package fencereentry is the fixture for IMPROVEMENT-PLAN §3.35 phase 4's
// deciding measurement: after the execution fence stops a real Go SDK guest,
// can the host re-enter that instance and get guest code to run?
//
// It needs two entry points in ONE module, because the whole question is about
// a second call into the instance the first call was stopped in. Two fixtures
// cannot express it: the host would instantiate twice, which is the
// fresh-instance problem §3.70 already measured and is not what phase 4 is
// about.
//
// See engine/fence_reentry_test.go.
package fencereentry

import (
	"strconv"

	"github.com/cleat-team/cleat/cleat"
)

// SpinForever burns wall-clock time and never enters the host, so the fence is
// what stops it and not a host call noticing a cancelled context. Same shape
// and same reasoning as testdata/spin -- the accumulator is returned so neither
// the Go compiler nor wasm-opt can prove the loop dead and delete it.
//
// The iteration count is not a duration. It is "far more than any fence budget
// a test would set", so that reaching the end is itself a test failure rather
// than a slow pass.
// Neither entry point takes an input, because neither needs one.
//
// They used to take an unused `input string` purely to work around a codegen
// defect: an entry point whose only parameter is h generated an
// `argsJSON := readString(...)` that nothing consumed, and the guest failed to
// compile with "declared and not used: argsJSON". That is fixed (#545), and
// testdata/noargs is its regression test -- so the workaround is gone rather
// than left in place with a comment explaining a problem that no longer
// exists.
func SpinForever(h cleat.HostCalls) (string, error) {
	// Registered before the loop, so the fence is guaranteed to stop this
	// workflow with a defer outstanding. This is phase 4's whole subject: a
	// cleanup that the guest's own defer runner (3.70) will never reach,
	// because the entry point never finishes.
	if _, err := h.DurableDeferFunc(func() {
		_, _ = h.DurableCall("fence-probe", "the_fenced_workflows_defer", `{}`)
	}); err != nil {
		return "", err
	}

	x := uint64(1)
	for i := 0; i < 100000000000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	return `{"value":` + strconv.FormatUint(x, 10) + `}`, nil
}

// AfterTheFence makes one host call and returns.
//
// The host call is the point. A defer body exists to reach the host -- release
// a lock, refund a payment -- so "the guest ran again" has to mean "the guest
// reached the host again", not "the export returned an int64". An export that
// returns without executing its body returns a perfectly plausible int64.
func AfterTheFence(h cleat.HostCalls) (string, error) {
	if _, err := h.DurableCall("fence-probe", "after_the_fence", `{}`); err != nil {
		return "", err
	}
	return `{"reentered":true}`, nil
}

// AllocateForever grows the guest heap until something refuses.
//
// It exists for the memory-limit arm of the abnormal-exit measurement. The
// slice is appended to a package-level sink so the Go compiler cannot prove the
// allocations dead and remove them -- the same reasoning as SpinForever's
// returned accumulator, and the same failure if it is dropped: the workflow
// would return promptly and the test would measure a clean exit while believing
// it measured an OOM.
//
// It registers a defer first, for the same reason SpinForever does: the
// question is whether an outstanding cleanup is still reachable afterwards.
func AllocateForever(h cleat.HostCalls) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		_, _ = h.DurableCall("fence-probe", "the_fenced_workflows_defer", `{}`)
	}); err != nil {
		return "", err
	}

	for i := 0; i < 1000000; i++ {
		sink = append(sink, make([]byte, 1<<20))
	}
	return `{"allocated":true}`, nil
}

// sink retains what AllocateForever allocates. Package-level and never read, so
// nothing can conclude the allocations are unnecessary.
var sink [][]byte

// SpinWithARunawayDefer registers a defer that never returns, then spins until
// the fence stops the entry point.
//
// It exists for the one safety property the host-side cleanup pass rests on:
// that pass grants fresh execution to a workflow the fence has already killed,
// so if the cleanup itself can run forever the fence has been undone rather
// than extended. A runaway defer is not a contrived case -- a cleanup that
// retries an unreachable service in a loop is the ordinary way to write one by
// accident.
func SpinWithARunawayDefer(h cleat.HostCalls) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		x := uint64(1)
		for i := 0; i < 100000000000; i++ {
			x = x*6364136223846793005 + 1442695040888963407
			x ^= x >> 33
		}
		sink = append(sink, []byte{byte(x)})
	}); err != nil {
		return "", err
	}

	x := uint64(1)
	for i := 0; i < 100000000000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	return `{"value":` + strconv.FormatUint(x, 10) + `}`, nil
}
