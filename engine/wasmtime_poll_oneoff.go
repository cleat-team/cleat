//go:build cgo

package engine

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// registerPollOneoff replaces wasmtime's poll_oneoff so a guest sleep is served
// by the DURABLE clock: satisfied at once while replaying, really waited when
// live, and advancing h.Now() either way. cleat#1633.
//
// WHY REPLACING IT IS SAFE WHEN REFUSING IT WAS NOT. cleat#1381 proposed
// refusing poll_oneoff and was reverted: the Go runtime parks a goroutine
// through it, so a trap replaced an out-of-memory failure with a WASI refusal
// and the host reported the killed workflow as SUCCEEDING. That is an argument
// against refusing, not against implementing -- a refusal has no correct answer
// for the scheduler's park, and an implementation does.
//
// MEASURED BEFORE BEING WRITTEN, because engine/wasi_policy.go records that a
// happy-path census cannot answer this: a probe counted poll_oneoff calls
// across three ordinary runs and found zero every time, since none was under
// memory pressure. Instrumenting the subscription SHAPES across a normal run
// and both OOM tests:
//
//	clock(id=1, rel, <1s) x4   -- and nothing else
//
// No fd_read, no fd_write, no empty subscription sets. Both
// TestAGuestKilledByTheMemoryLimitIsNotReportedAsSuccess and
// TestTheHostRunsDefersOfAnOOMKilledWorkflow pass with this installed, which is
// the known-positive for the whole change rather than a closing formality.
//
// The semantics mirror wazero's imports/wasi_snapshot_preview1/poll.go, a
// working implementation of the same ABI, so the arms nothing exercised here
// still answer the way a guest would expect: a clock subscription is
// acknowledged and the largest timeout observed, fd_write is ENOTSUP, fd_read
// is acknowledged.
func (b *wasmtimeBackend) registerPollOneoff(linker *wasmtime.Linker) error {
	const subSize = 48
	const evtSize = 32
	linker.AllowShadowing(true)
	defer linker.AllowShadowing(false)
	return b.hostFunc(linker, "wasi_snapshot_preview1", "poll_oneoff",
		func(caller *wasmtime.Caller, inPtr, outPtr int32, nsubs int32, neventsPtr int32) int32 {
			mem := caller.GetExport("memory")
			if mem == nil || mem.Memory() == nil {
				return wasiErrnoFault
			}
			data := mem.Memory().UnsafeData(caller)
			if nsubs < 0 {
				return 28 // EINVAL
			}

			var maxTimeout uint64
			var nevents uint32
			for i := int32(0); i < nsubs; i++ {
				off := int(inPtr) + int(i)*subSize
				if off+subSize > len(data) {
					return wasiErrnoFault
				}
				userdata := binary.LittleEndian.Uint64(data[off:])
				et := data[off+8]
				eOff := int(outPtr) + int(nevents)*evtSize
				if eOff+evtSize > len(data) {
					return wasiErrnoFault
				}
				for k := eOff; k < eOff+evtSize; k++ {
					data[k] = 0
				}
				binary.LittleEndian.PutUint64(data[eOff:], userdata)
				var errno uint16
				switch et {
				case 0: // clock
					if t := binary.LittleEndian.Uint64(data[off+24:]); t > maxTimeout {
						maxTimeout = t
					}
				case 2: // fd_write
					errno = 58 // ENOTSUP
				}
				binary.LittleEndian.PutUint16(data[eOff+8:], errno)
				data[eOff+10] = et
				nevents++
			}
			if int(neventsPtr)+4 > len(data) {
				return wasiErrnoFault
			}
			binary.LittleEndian.PutUint32(data[neventsPtr:], nevents)
			// THE DURABLE CLOCK DECIDES, not a replay flag. See
			// HostHandler.ServeWasiSleep. A zero here is "the wait already
			// happened", which covers replay and resumed-after-downtime with
			// one predicate.
			if maxTimeout > 0 {
				d := time.Duration(maxTimeout)
				if b.handler != nil {
					d = b.handler.ServeWasiSleep(context.Background(),
						int64(maxTimeout)/int64(time.Millisecond))
				}
				sleepBounded(context.Background(), d)
			}
			return 0
		})
}
