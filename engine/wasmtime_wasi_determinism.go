//go:build cgo

package engine

import (
	"context"
	"encoding/binary"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// WASI preview1 clockid values. Only the first two matter here; the process and
// thread CPU clocks are not something a workflow has any business reading and
// fall through to the monotonic branch.
const (
	wasiClockRealtime  int32 = 0
	wasiClockMonotonic int32 = 1
)

// wasiErrnoFault is WASI preview1's EFAULT (bad address). Position 21 in the
// errno enum, counted rather than recalled.
const wasiErrnoFault int32 = 21

// registerWasiDeterminism replaces WASI's clock and entropy with replayable
// sources, AFTER DefineWasi has bound the originals.
//
// WHY THE TWO CLOCK IDS ARE SOURCED SEPARATELY, which is the whole design.
// The obvious implementation feeds the durable clock to both. That is the
// configuration measured above as killing the guest: a workflow that sleeps an
// hour durably makes the durable clock jump an hour, and the GC pacer reacts to
// the jump rather than to the value. So:
//
//	CLOCK_REALTIME  -> the durable clock. This is what workflow code sees, it is
//	                   recorded and replayable, and it may jump freely. The Go
//	                   runtime never reads it -- measured at zero reads across
//	                   every guest tried, on both backends.
//	CLOCK_MONOTONIC -> a synthetic counter, smooth, never zero, never backwards,
//	                   and carrying no relationship to wall-clock time. It is
//	                   still replayable: it is a pure function of how many times
//	                   the guest has asked.
//
// Both are deterministic, which is the point; they are deterministic in
// different ways because they answer different questions.
func (b *wasmtimeBackend) registerWasiDeterminism(linker *wasmtime.Linker) error {
	// Shadowing is off by default and registerWasiStubs relies on that: it
	// registers environ_get/environ_sizes_get with `_ =` specifically because a
	// duplicate-definition error there is benign. Turn it on only around these
	// two, so an accidental redefinition elsewhere still fails loudly.
	linker.AllowShadowing(true)
	defer linker.AllowShadowing(false)

	if err := b.hostFunc(linker, "wasi_snapshot_preview1", "clock_time_get",
		func(caller *wasmtime.Caller, id int32, precision int64, resultPtr int32) int32 {
			var ns int64
			if id == wasiClockRealtime {
				if b.handler != nil {
					// Now() is milliseconds since the epoch; WASI wants nanos.
					ns = b.handler.Now(context.Background()) * 1_000_000
				}
			} else {
				b.wasiMonotonicNs += wasiMonotonicStepNs
				ns = b.wasiMonotonicNs
			}
			return writeU64(caller, resultPtr, uint64(ns))
		}); err != nil {
		return err
	}

	return b.hostFunc(linker, "wasi_snapshot_preview1", "random_get",
		func(caller *wasmtime.Caller, buf int32, bufLen int32) int32 {
			if bufLen < 0 {
				return wasiErrnoFault
			}
			mem := caller.GetExport("memory")
			if mem == nil || mem.Memory() == nil {
				return wasiErrnoFault
			}
			data := mem.Memory().UnsafeData(caller)
			if int64(buf)+int64(bufLen) > int64(len(data)) {
				return wasiErrnoFault
			}
			// Random() yields 8 deterministic bytes per call, seeded from the
			// workflow id and step, so replay reproduces the stream exactly.
			var word [8]byte
			for i := int32(0); i < bufLen; i++ {
				if i%8 == 0 {
					v := uint64(0)
					if b.handler != nil {
						v = uint64(b.handler.Random(context.Background()))
					}
					binary.BigEndian.PutUint64(word[:], v)
				}
				data[buf+i] = word[i%8]
			}
			return 0
		})
}

// writeU64 stores a little-endian u64 at ptr in the caller's memory, returning
// a WASI errno.
func writeU64(caller *wasmtime.Caller, ptr int32, v uint64) int32 {
	mem := caller.GetExport("memory")
	if mem == nil || mem.Memory() == nil {
		return wasiErrnoFault
	}
	data := mem.Memory().UnsafeData(caller)
	if int64(ptr) < 0 || int64(ptr)+8 > int64(len(data)) {
		return wasiErrnoFault
	}
	binary.LittleEndian.PutUint64(data[ptr:ptr+8], v)
	return 0
}
