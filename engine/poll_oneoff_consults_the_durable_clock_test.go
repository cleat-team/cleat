//go:build cgo

package engine

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// recordingSleepHandler answers ServeWasiSleep with a fixed duration and
// remembers what it was asked. It is NOT stubHostHandler, deliberately: that
// stub returns 0 for every sleep, so a test using it could not tell "the host
// consulted the durable clock and was told the wait had happened" from "the
// host never consulted anything".
type recordingSleepHandler struct {
	stubHostHandler
	askedMs []int64
	answer  time.Duration
}

func (h *recordingSleepHandler) ServeWasiSleep(_ context.Context, durationMs int64) time.Duration {
	h.askedMs = append(h.askedMs, durationMs)
	return h.answer
}

// poll_oneoff routes a guest's clock subscription through the DURABLE clock.
// cleat#1633.
//
// WHY THIS EXISTS SEPARATELY FROM TestAReplayedGuestSleepDoesNotWaitAgain.
// That test drives ServeWasiSleep directly and proves the PREDICATE. It says
// nothing about the wiring, and I checked rather than assumed: deleting the
// ServeWasiSleep call from registerPollOneoff leaves it entirely green. Two
// sites, one of them asserted by nothing, is the shape that let cleat#1057
// leave the int binding arm reporting success.
//
// It drives poll_oneoff through a WAT module rather than a compiled Go guest
// because a Go guest cannot reach it on purpose: time.Sleep in workflow code is
// vet error E004, so the routes that land here are the Go runtime's own and
// cannot be written into a fixture.
func TestPollOneoffConsultsTheDurableClock(t *testing.T) {
	const wat = `(module
  (import "wasi_snapshot_preview1" "poll_oneoff"
     (func $poll (param i32 i32 i32 i32) (result i32)))
  (memory (export "memory") 1)
  (func (export "run") (result i32)
     (call $poll (i32.const 0) (i32.const 256) (i32.const 1) (i32.const 512))))`

	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("wasmtime backend unavailable: %v", err)
	}
	defer b.Close(ctx)

	run := func(t *testing.T, h *recordingSleepHandler, timeoutMs int64) time.Duration {
		t.Helper()
		b.handler = h

		store := wasmtime.NewStore(b.engine)
		defer store.Close()
		store.SetEpochDeadline(1 << 32)
		store.SetWasi(wasmtime.NewWasiConfig())

		linker := wasmtime.NewLinker(b.engine)
		if err := linker.DefineWasi(); err != nil {
			t.Fatalf("DefineWasi: %v", err)
		}
		if err := b.registerPollOneoff(linker); err != nil {
			t.Fatalf("registerPollOneoff: %v", err)
		}

		bin, err := wasmtime.Wat2Wasm(wat)
		if err != nil {
			t.Fatalf("wat2wasm: %v", err)
		}
		mod, err := wasmtime.NewModule(b.engine, bin)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		defer mod.Close()
		inst, err := linker.Instantiate(store, mod)
		if err != nil {
			t.Fatalf("instantiate: %v", err)
		}

		mem := inst.GetExport(store, "memory").Memory()
		data := mem.UnsafeData(store)
		for i := range data[:600] {
			data[i] = 0
		}
		// One CLOCK_MONOTONIC subscription, relative, timeoutMs.
		binary.LittleEndian.PutUint64(data[0:], 0xABCD)                       // userdata
		data[8] = 0                                                           // eventtype = clock
		binary.LittleEndian.PutUint32(data[16:], 1)                           // CLOCK_MONOTONIC
		binary.LittleEndian.PutUint64(data[24:], uint64(timeoutMs)*1_000_000) // timeout ns
		binary.LittleEndian.PutUint64(data[32:], 1)                           // precision

		fn := inst.GetExport(store, "run").Func()
		start := time.Now()
		res, err := fn.Call(store)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("poll_oneoff: %v", err)
		}
		if errno, ok := res.(int32); !ok || errno != 0 {
			t.Fatalf("poll_oneoff returned errno %v, want 0", res)
		}
		if got := binary.LittleEndian.Uint32(mem.UnsafeData(store)[512:]); got != 1 {
			t.Errorf("nevents = %d, want 1: the clock subscription was not acknowledged, so a "+
				"guest would poll again forever", got)
		}
		return elapsed
	}

	t.Run("the durable clock is asked, with the guest's own duration", func(t *testing.T) {
		h := &recordingSleepHandler{answer: 0}
		run(t, h, 500)
		if len(h.askedMs) != 1 {
			t.Fatalf("ServeWasiSleep was called %d times, want 1.\n\n"+
				"poll_oneoff is not consulting the durable clock at all, so a replayed "+
				"guest sleep waits again -- cleat#1633 exactly.", len(h.askedMs))
		}
		if h.askedMs[0] != 500 {
			t.Errorf("ServeWasiSleep was asked for %dms, want 500: poll_oneoff is converting "+
				"the guest's timeout wrongly", h.askedMs[0])
		}
	})

	t.Run("a wait the durable clock says already happened is not waited", func(t *testing.T) {
		h := &recordingSleepHandler{answer: 0}
		elapsed := run(t, h, 500)
		if elapsed > 200*time.Millisecond {
			t.Errorf("a replayed 500ms sleep took %v; it must return at once.\n\n"+
				"Every production segment replays its history before continuing, so this "+
				"is paid on every resume, forever.", elapsed)
		}
	})

	// THE CONTROL. Without it, "returns at once" is also satisfied by a
	// poll_oneoff that never sleeps for anyone -- which is what the code did
	// before cleat#1300 and is not the behaviour being asserted.
	t.Run("a wait the durable clock says is still ahead IS waited", func(t *testing.T) {
		h := &recordingSleepHandler{answer: 300 * time.Millisecond}
		elapsed := run(t, h, 500)
		if elapsed < 250*time.Millisecond {
			t.Errorf("a live sleep took %v, but the durable clock asked for 300ms.\n\n"+
				"poll_oneoff is ignoring the duration it was handed, so a live guest sleep "+
				"does not wait -- the other half of the same defect.", elapsed)
		}
	})
}
