// Package spin provides a workflow that burns wall-clock time and nothing
// else. It exists so the engine's execution-time fence can be tested against a
// workload that genuinely runs long.
//
// Why not reuse testdata/basic's LongRunning: that loops on DurableCall, and
// each durable call costs roughly 2.9 KB of host memory for the event it
// records. Running one for a full second means ~170k calls and ~500 MB of
// heap, which is not something a test should ask a CI runner for. A pure
// arithmetic loop costs nothing per iteration and is interrupted by exactly
// the same mechanism: wasmtime's epoch interruption instruments loop
// backedges, so it fires whether or not the guest ever calls into the host.
//
// See IMPROVEMENT-PLAN.md 2.10.
package spin

import (
	"strconv"

	"github.com/cleat-team/cleat/cleat"
)

// Spin iterates a pure arithmetic loop `iterations` times and returns the
// accumulated value.
//
// The result is returned rather than discarded so that neither the Go compiler
// nor wasm-opt can prove the loop dead and delete it -- a spin loop that gets
// optimised away would make any fence test pass instantly and for the wrong
// reason, which is the exact failure this fixture was written to stop
// happening a second time.
//
// h is unused: the point is a workload that never enters the host, so that the
// fence is what stops it and not a host call observing a cancelled context.
func Spin(h cleat.HostCalls, iterations int) (string, error) {
	x := uint64(1)
	for i := 0; i < iterations; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	return strconv.FormatUint(x, 10), nil
}
