//go:build cgo

package engine

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// skipIfNotNeeded returns true if the named import should be skipped because
// the WASM module doesn't import it and we have a valid import list.
func (b *wasmtimeBackend) skipIfNotNeeded(name string) bool {
	return b.envNeeded != nil && !b.envNeeded[name]
}

func (b *wasmtimeBackend) registerCleatCall(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	if b.skipIfNotNeeded("cleat_call") {
		return nil
	}
	return b.hostFunc(linker, "env", "cleat_call", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen int32) int64 {
		t0 := time.Now()
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return badParamDurableCall
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return badParamDurableCall
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return badParamDurableCall
		}
		// wasmtimeReadPayload, not wasmtimeReadStringValidated: a durable call
		// that takes no arguments passes "" here and must be allowed.
		req, ok := wasmtimeReadPayload(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return badParamDurableCall
		}
		t1 := time.Now()
		callCtx := ctxWithMem(context.Background(), buf)
		result := h.DurableCall(callCtx, nil, service, op, req, uint32(respPtr), uint32(respMaxLen))
		elapsed := time.Since(t0)
		if DebugTiming && elapsed > 5*time.Millisecond {
			fmt.Fprintf(os.Stderr, "TIMING-WASMTIME: cleat_call total=%dms read=%dus handler=%dus\n",
				elapsed.Milliseconds(), t1.Sub(t0).Microseconds(), time.Since(t1).Microseconds())
		}
		return result
	})
}

func (b *wasmtimeBackend) registerCleatComplete(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	if b.skipIfNotNeeded("cleat_complete") {
		return nil
	}
	return b.hostFunc(linker, "env", "cleat_complete", func(caller *wasmtime.Caller,
		status int32, resultPtr int32, resultLen int32) int64 {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		if resultLen > 0 && uint64(resultPtr)+uint64(resultLen) <= uint64(len(buf)) {
			result := string(buf[resultPtr : resultPtr+resultLen])
			if status == 0 {
				*completeResult = result
			} else {
				*completeErr = result
			}
		}
		return 0
	})
}

func (b *wasmtimeBackend) registerCleatPollWork(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_poll_work") {
		return nil
	}
	return b.hostFunc(linker, "env", "cleat_poll_work", func(caller *wasmtime.Caller,
		entryNamePtr int32, entryNameMaxLen int32,
		argsPtr int32, argsMaxLen int32) int64 {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}

		// Both destinations are guest-supplied and must be range-checked
		// before either is written. Doing it up front also makes the call
		// all-or-nothing: a guest that gets its second pointer wrong does not
		// come back to a half-populated first buffer.
		entryBytes := []byte(b.workEntryPoint)
		entryLen := clampToMaxLen(len(entryBytes), entryNameMaxLen)
		argsLen := clampToMaxLen(len(b.workInput), argsMaxLen)
		if !guestRangeOK(buf, entryNamePtr, entryLen) || !guestRangeOK(buf, argsPtr, argsLen) {
			return errBadParamInt64
		}

		if entryLen > 0 {
			copy(buf[uint32(entryNamePtr):], entryBytes[:entryLen])
		}
		if argsLen > 0 {
			copy(buf[uint32(argsPtr):], b.workInput[:argsLen])
		}
		return int64(argsLen) | int64(entryLen)<<32
	})
}

// The two arms of the soft kill.
const (
	interruptArmNone = iota
	interruptArmFuel
	interruptArmTime
)

// ---------------------------------------------------------------------------------------------
// cleat#3922: the soft kill's stop bit.
//
// A guest that loops for ever is stopped by wasmtime's fuel trap at whatever instruction it happens
// to be on. That kills the INSTANCE mid-borrow: rquickjs's RefCell guards never run their Drop, so
// the host's post-kill drain panics ("RefCell already borrowed") and no defer body runs.
//
// A QuickJS interrupt reaches the same guest through a different door. The bytecode loop calls the
// handler installed by the plugin; returning true raises an exception QuickJS will not deliver to
// user code, the Rust call frames return rather than being abandoned, and what reaches the host is
// javy's process::abort() -- a trap, but on an instance with nothing left borrowed. The drain then
// works.
//
// The stop bit is FUEL-based: answer 1 when the remaining budget is at or below a reserve, so the
// guest stops cleanly with enough left for the host to finish. It is answered 0 for the whole of a
// drain (interruptFenced): the drain is the work this exists to make possible.
// ---------------------------------------------------------------------------------------------

// Measurement of the observation cadence, for reporting the two numbers the margin is derived from.
// Reset with MeasureInterrupt and read with InterruptStats. Not used by the policy itself.
var (
	interruptPolls   atomic.Int64
	interruptLast    atomic.Uint64
	interruptMaxGap  atomic.Uint64
	interruptGapSeen atomic.Bool
)

// Measurement state for the two margins and the unwind. These are package-level and every
// execution writes them, so they are ATOMICS: the first version used plain fields on the reasoning
// that they were only touched from the guest's own thread, which is true and irrelevant — two
// executions in one process share them, and `go test -race` with two concurrent kills reported a
// data race between the read and the write. The numbers are only read by the measurement harness,
// which runs one execution at a time; under concurrency they are approximate, which is fine for a
// measurement and not fine for a data race.
var (
	interruptLastPollAt atomic.Int64 // UnixNano of the previous handler call; 0 = none yet
	interruptMaxGapTime atomic.Int64 // nanos
	interruptFiredAt    atomic.Int64 // UnixNano of the last soft fire; 0 = none outstanding
	interruptFiredFuel  atomic.Uint64
	interruptUnwind     atomic.Int64 // nanos
	interruptUnwindFuel atomic.Uint64
	interruptUnwinds    atomic.Int64
)

func storeMax(dst *atomic.Int64, v int64) {
	for {
		old := dst.Load()
		if v <= old || dst.CompareAndSwap(old, v) {
			return
		}
	}
}

func storeMaxUint(dst *atomic.Uint64, v uint64) {
	for {
		old := dst.Load()
		if v <= old || dst.CompareAndSwap(old, v) {
			return
		}
	}
}

// recordUnwind is called by Execute once the guest's error is in hand.
func recordUnwind(store *wasmtime.Store) {
	fired := interruptFiredAt.Swap(0)
	if fired == 0 {
		return
	}
	storeMax(&interruptUnwind, int64(time.Since(time.Unix(0, fired))))
	if f := interruptFiredFuel.Load(); f > 0 && store != nil {
		if left, err := store.GetFuel(); err == nil && f > left {
			storeMaxUint(&interruptUnwindFuel, f-left)
		}
	}
	interruptUnwinds.Add(1)
}

// MeasureInterrupt resets the cadence measurement.
func MeasureInterrupt() {
	interruptPolls.Store(0)
	interruptMaxGap.Store(0)
	interruptGapSeen.Store(false)
	interruptLastPollAt.Store(0)
	interruptMaxGapTime.Store(0)
	interruptFiredAt.Store(0)
	interruptFiredFuel.Store(0)
	interruptUnwind.Store(0)
	interruptUnwindFuel.Store(0)
	interruptUnwinds.Store(0)
}

// InterruptStats reports the number of handler calls and the largest fall in remaining fuel between
// two consecutive calls.
func InterruptStats() (polls int64, maxGapFuel uint64, maxGapTime, maxUnwind time.Duration, maxUnwindFuel uint64) {
	return interruptPolls.Load(), interruptMaxGap.Load(),
		time.Duration(interruptMaxGapTime.Load()), time.Duration(interruptUnwind.Load()), interruptUnwindFuel.Load()
}

func (b *wasmtimeBackend) registerCleatInterruptRequested(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_interrupt_requested") {
		return nil
	}
	return b.hostFunc(linker, "env", "cleat_interrupt_requested", func(*wasmtime.Caller) int32 {
		if b.interruptFenced || b.store == nil {
			return 0
		}
		interruptPolls.Add(1)
		now := time.Now().UnixNano()
		if prev := interruptLastPollAt.Swap(now); prev != 0 {
			storeMax(&interruptMaxGapTime, now-prev)
		}
		// The epoch arm. This is the arm that matters by default: --wasm-instruction-limit is 0, so
		// fuel is not metered and the kill is the epoch fence. The budget read here is the one the
		// fence will fire against -- the host has just charged the guest for the run that ended and
		// the guest is not running -- so it inherits both the re-arming around host calls and
		// configureStore's switch to ctx's remaining deadline.
		if b.interruptMarginTime > 0 && b.budget != nil && b.budget.guestRemaining() <= b.interruptMarginTime {
			b.interruptRequested = true
			b.interruptArm = interruptArmTime
			interruptFiredAt.Store(time.Now().UnixNano())
			// Make room for the unwind: the bracket re-arms the deadline from the budget, and a
			// one-tick deadline can expire while the guest unwinds. See interruptUnwindGrant.
			b.grantUnwind()
			return 1
		}
		remaining, err := b.store.GetFuel()
		if err != nil {
			// Fuel is not enabled for this execution (no instruction limit configured): the epoch
			// arm above is the only one available.
			return 0
		}
		previous := interruptLast.Swap(remaining)
		// The first call has no previous value to subtract from; it only establishes the baseline.
		if interruptGapSeen.Swap(true) && previous > remaining {
			if gap := previous - remaining; gap > interruptMaxGap.Load() {
				interruptMaxGap.Store(gap)
			}
		}
		margin := b.limits.interruptMargin
		if margin > 0 && remaining <= margin {
			b.interruptRequested = true
			b.interruptArm = interruptArmFuel
			interruptFiredAt.Store(time.Now().UnixNano())
			if f, err := b.store.GetFuel(); err == nil {
				interruptFiredFuel.Store(f)
			}
			return 1
		}
		return 0
	})
}
