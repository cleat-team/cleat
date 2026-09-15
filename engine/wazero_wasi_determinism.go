package engine

import (
	"context"
	"encoding/binary"
	"io"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// wasiMonotonicFloorNs is the smallest amount CLOCK_MONOTONIC advances between
// two reads.
//
// It exists only to keep the clock STRICTLY increasing and never zero. The Go
// runtime throws "fatal error: nanotime returning zero" before user code runs,
// and two reads inside the same nanosecond are ordinary on a fast machine.
//
// IT IS NOT A STEP SIZE, and the difference is the whole of cleat#1300. This
// used to be wasiMonotonicStepNs = 1ms, ADDED PER READ, making the clock "a
// pure function of how many times the guest has asked". That is replayable and
// it multiplied every guest sleep on wasmtime by ~125x.
const wasiMonotonicFloorNs int64 = 1

// nextMonotonicNs returns the value CLOCK_MONOTONIC should report, given what
// it last reported and when this execution began.
//
// ONE DEFINITION FOR BOTH BACKENDS. The wazero and wasmtime halves of
// cleat#1300 "must agree", and the way they previously agreed was by each
// adding the same constant -- which is agreement by coincidence, and neither
// side's test would have noticed the other drifting. This is the rule itself,
// so a divergence is a compile error rather than a behaviour difference.
//
// WHY REAL ELAPSED TIME. Go's usleep passes poll_oneoff a RELATIVE timeout
// computed as deadline - nanotime() (runtime/os_wasip1.go), and wasmtime's
// poll_oneoff honours it exactly -- measured 500ms -> 502ms. Under a clock
// advancing a fixed step per read, the guest slept the full duration, woke
// believing one step had passed, and slept again. Measured end to end with a
// real Go guest on wasmtime:
//
//	real host clock                504ms
//	1ms per read (what shipped)    62.5 SECONDS
//	real elapsed (this)            504ms
//
// The fixed step was measured on WAZERO, where sleeping is free --
// platform.FakeNanosleep "implements sys.Nanosleep by returning without
// sleeping" -- so another iteration cost only a host crossing. One constant,
// two backends, opposite cost models, and it was tuned against the one that
// does not ship.
//
// AND IT IS THE SAFE ANSWER TO THE GC-PACER FAILURE, not a risk against it.
// That failure -- 3 collections instead of 17, a 256MB heap against a 32MB
// limit -- came from a clock advancing a second or more PER READ, i.e. time
// running faster than reality. A clock tracking real time is what a native Go
// process has and what the pacer is designed for.
//
// THIS CLOCK IS NO LONGER REPLAYABLE, DELIBERATELY. The old one was, and its
// doc said so. CLOCK_REALTIME is the replayable domain -- it is the durable
// clock, it is what workflow code sees through h.Now(), and it is recorded.
// CLOCK_MONOTONIC exists so timers terminate; a workflow that reaches it is
// already outside the deterministic surface, and vet refuses the routes that
// get there (E003 time.Now, E004 time.Sleep, E014 After/NewTicker/NewTimer).
// Making it replayable bought nothing and cost 125x.
func nextMonotonicNs(prevNs int64, start, now time.Time) int64 {
	elapsed := now.Sub(start).Nanoseconds()
	if elapsed < prevNs+wasiMonotonicFloorNs {
		elapsed = prevNs + wasiMonotonicFloorNs
	}
	return elapsed
}

// withDeterministicClockAndEntropy attaches replayable time and randomness to a
// GUEST module config. It is the wazero half of cleat#1300; the wasmtime half
// is registerWasiDeterminism, and the two must agree.
//
// On the guest's config, not the WASI host module's, because that is where
// wazero resolves them from: imports/wasi_snapshot_preview1/clock.go reads
// `mod.(*wasm.ModuleInstance).Sys` -- the CALLING module. cleat configured the
// WASI module instead, which is why its zero-clock settings never applied to
// anything and `time.Now()` returned wazero's default fake 2022 date.
//
// The two clock domains are sourced separately and the difference is load
// bearing; see registerWasiDeterminism for the measurements behind it. In
// short: realtime may jump, monotonic may not.
func withDeterministicClockAndEntropy(ctx context.Context, config wazero.ModuleConfig) wazero.ModuleConfig {
	h := handlerFromContextOrNil(ctx)

	// Per-instantiation, so each module gets its own monotonic sequence
	// starting near zero, as a freshly started process does.
	var monotonicNs int64
	monotonicStart := time.Now()

	return config.
		WithWalltime(func() (int64, int32) {
			if h == nil {
				return 0, 0
			}
			ms := h.Now(context.Background())
			return ms / 1000, int32((ms % 1000) * 1_000_000)
		}, sys.ClockResolution(1_000_000)).
		WithNanotime(func() int64 {
			// REAL ELAPSED TIME, NOT A COUNT OF READS. cleat#1300.
			//
			// This was `monotonicNs += wasiMonotonicStepNs` -- 1ms per read,
			// "a pure function of how many times the guest has asked". That is
			// replayable, and on wasmtime it multiplies a sleep by 125x.
			//
			// Go's usleep passes a RELATIVE timeout computed as
			// deadline - nanotime() (runtime/os_wasip1.go). wasmtime's
			// poll_oneoff honours it exactly -- measured 500ms -> 502ms. So
			// under a clock advancing 1ms per read the guest sleeps ~500ms,
			// wakes believing 1ms passed, and sleeps ~500ms again: a real
			// 500ms time.Sleep took 62.5 SECONDS end to end, against 504ms on
			// the real host clock.
			//
			// The 1ms step was measured on WAZERO, where sleeping is free --
			// platform.FakeNanosleep "implements sys.Nanosleep by returning
			// without sleeping" -- so another iteration costs only a host
			// crossing. One constant, two backends, opposite cost models.
			//
			// Real elapsed time is also the SAFEST answer to the GC-pacer
			// failure recorded above, not a risk against it: that failure was
			// a clock advancing a second or more PER READ, i.e. faster than
			// reality. A clock tracking real time is what a native Go process
			// has.
			//
			// The floor keeps the two properties the old counter had for free
			// and that the Go runtime requires: never zero -- "fatal error:
			// nanotime returning zero" -- and never backwards.
			monotonicNs = nextMonotonicNs(monotonicNs, monotonicStart, time.Now())
			return monotonicNs
		}, sys.ClockResolution(1_000_000)).
		WithNanosleep(func(ns int64) {
			// poll_oneoff DOES NOT SPIN BY NATURE -- it spun because nothing was
			// here. With only clock subscriptions it calls sysCtx.Nanosleep(timeout)
			// and returns (imports/wasi_snapshot_preview1/poll.go), and wazero's
			// default is platform.FakeNanosleep, which "implements sys.Nanosleep by
			// returning without sleeping". So the guest's Go runtime re-read
			// nanotime, found its deadline still ahead, and called again.
			//
			// AN OPTIMISATION, NOT A CORRECTNESS REQUIREMENT, and the distinction
			// is worth stating because the reverse is easy to assume. Under the
			// real-elapsed clock above, a guest with no Nanosleep BUSY-WAITS and
			// still finishes after the right wall-clock duration -- wasteful, not
			// wrong. It was the FROZEN clock that livelocked (51,177,005 calls in
			// twenty seconds), because then no amount of waiting moved the deadline.
			//
			// This also makes wazero match wasmtime, whose poll_oneoff really
			// blocks -- measured 500ms -> 502ms. Two backends that sleep for the
			// same reason rather than one sleeping and one spinning.
			//
			// THE DURABLE CLOCK DECIDES, the same predicate wasmtime's
			// poll_oneoff uses (engine/wasmtime_poll_oneoff.go). cleat#1633
			// landed this on BOTH backends at once deliberately: doing it here
			// alone would make h.Now() advance across a guest sleep under
			// cleatctl replay and not in production, which is cleat#1300's own
			// complaint with the roles swapped.
			d := time.Duration(ns)
			if h != nil {
				d = h.ServeWasiSleep(ctx, ns/int64(time.Millisecond))
			}
			sleepBounded(ctx, d)
		}).
		WithRandSource(&deterministicEntropy{h: h})
}

// deterministicEntropy supplies random_get from the same seeded source the
// sanctioned cleat_random host call uses, so a guest reaching entropy through
// WASI gets something replayable instead of the host's real generator.
type deterministicEntropy struct {
	h HostHandler
}

func (d *deterministicEntropy) Read(p []byte) (int, error) {
	var word [8]byte
	for i := range p {
		if i%8 == 0 {
			v := uint64(0)
			if d.h != nil {
				v = uint64(d.h.Random(context.Background()))
			}
			binary.BigEndian.PutUint64(word[:], v)
		}
		p[i] = word[i%8]
	}
	return len(p), nil
}

var _ io.Reader = (*deterministicEntropy)(nil)

// sleepBounded blocks for d, or until ctx is done, whichever comes first.
//
// BOUNDED BECAUSE A GUEST CHOOSES d. time.Sleep in workflow code is already a
// vet error (E004, internal/closure/closure.go), so what reaches here is a
// sleep vet's pattern list does not name -- a dependency's retry loop, Go
// runtime internals. Those are short in practice, and "in practice" is not a
// bound: without this, one pathological sleep hangs a `cleat dev` run with no
// way out, and the existing call timeout could not cut it short because the
// goroutine is parked rather than executing.
//
// Deliberately NOT a refusal. The caller has already advanced the durable
// clock; returning early on a cancelled context lets the run unwind through
// the normal timeout path rather than inventing a second failure mode here.
func sleepBounded(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
