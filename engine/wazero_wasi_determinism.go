package engine

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

// wasiMonotonicStepNs is how far the synthetic monotonic clock moves per read.
//
// THIS NUMBER IS A CORRECTNESS PARAMETER, NOT A TUNING KNOB, and both directions
// have measured failures (cleat#1300).
//
// TOO SLOW hangs the guest. `poll_oneoff` does not block; it spins on nanotime
// until the deadline passes. Measured on a guest sleeping 500ms: 509 host calls
// at 1ms per read, versus 51,177,005 calls in twenty seconds and no completion
// when the clock does not advance at all. At 1us per read the same timer needs
// 500,000 crossings of the host boundary.
//
// TOO FAST breaks the garbage collector. Go's pacer reads this clock, and a
// clock that leaps makes it conclude a great deal of time has passed. Measured
// on a guest allocating 256MB with a small live set: 17 GC cycles and a 32MB
// final heap under a smoothly advancing clock, against 3 cycles and a 256MB
// heap when the clock jumped a second or more per step. DefaultMemoryLimitPages
// is 512 pages -- 32MB -- so the second outcome is not slower, it is killed.
// The effect is a threshold and not a proportion: +1s, +1h and +1d per step all
// produced exactly 3 cycles and 256MB.
//
// 1ms per read sits in the measured-good band. It is deliberately NOT derived
// from the durable clock; see the realtime branch below.
const wasiMonotonicStepNs int64 = 1_000_000

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

	// Per-instantiation, so each module gets its own monotonic sequence and
	// each replay of that module reproduces it.
	var monotonicNs int64

	return config.
		WithWalltime(func() (int64, int32) {
			if h == nil {
				return 0, 0
			}
			ms := h.Now(context.Background())
			return ms / 1000, int32((ms % 1000) * 1_000_000)
		}, sys.ClockResolution(1_000_000)).
		WithNanotime(func() int64 {
			monotonicNs += wasiMonotonicStepNs
			return monotonicNs
		}, sys.ClockResolution(1_000_000)).
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
