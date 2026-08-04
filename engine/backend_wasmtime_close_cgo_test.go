//go:build cgo

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// TestWasmtimeCloseJoinsEpochTicker pins the shutdown ordering that caused a
// CI panic on 2026-08-03:
//
//	panic: object has been closed already
//	  wasmtime-go.(*Engine).IncrementEpoch
//	  engine.(*wasmtimeBackend).startEpochTicker.func1
//
// Close used to close(epochStop) and then call engine.Close() immediately.
// Closing the channel is a *request* to stop, not an acknowledgement that the
// goroutine has stopped -- a goroutine already committed to the `case
// <-ticker.C` branch goes on to call IncrementEpoch on a freed engine.
//
// The window is one scheduling quantum wide once every epochTickInterval, so
// it surfaced as a rare flake rather than a reliable failure. Rather than try
// to hit that window, this asserts the ordering guarantee that closes it:
// Close does not return until the ticker goroutine has exited.
//
// The first attempt at this test called NewWasmtimeBackend, then Close, then
// checked whether epochDone was closed. It passed with the join removed --
// once epochStop closes, the real goroutine almost always gets scheduled and
// exits before the check runs, so the test measured scheduler luck rather
// than the ordering guarantee. It is written instead against a backend whose
// "ticker goroutine" is this test, so the wait is observable directly.
func TestWasmtimeCloseJoinsEpochTicker(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	if b.epochDone == nil {
		t.Fatal("epochDone is nil on the root backend; Close cannot join the ticker")
	}
	// Stop the real ticker before standing in for it, so the engine below is
	// not being touched by a live goroutine.
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close (real backend): %v", err)
	}

	// A backend with no goroutine running: epochDone stays open until this
	// test closes it, so "did Close wait?" becomes directly observable.
	stub := &wasmtimeBackend{
		engine:    wasmtime.NewEngine(),
		epochStop: make(chan struct{}),
		epochDone: make(chan struct{}),
	}

	returned := make(chan struct{})
	go func() { defer close(returned); _ = stub.Close(ctx) }()

	select {
	case <-returned:
		t.Fatal("Close returned without waiting for the epoch ticker to exit -- " +
			"engine.Close() can then free the engine out from under IncrementEpoch")
	case <-time.After(200 * time.Millisecond):
		// Correct: still blocked on the join.
	}

	close(stub.epochDone) // the ticker goroutine has now "exited"

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the ticker exited")
	}
}

// TestWasmtimeCloseIsIdempotent covers the documented requirement that a
// second Close does not panic on the already-closed epochStop, and does not
// deadlock on an already-drained epochDone.
func TestWasmtimeCloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	done := make(chan struct{})
	go func() { defer close(done); _ = b.Close(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Close blocked; idempotency is broken")
	}
}

// TestWasmtimeCloseUnderTickPressure exercises the real shutdown path
// repeatedly with a live ticker, which is where the original panic was seen.
// It is not a reliable reproducer of the race on its own -- that is the point
// of the invariant test above -- but it does run the actual code path, and
// under -race it also covers concurrent access to the engine handle.
func TestWasmtimeCloseUnderTickPressure(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		b, err := NewWasmtimeBackend(ctx)
		if err != nil {
			t.Fatalf("iteration %d: NewWasmtimeBackend: %v", i, err)
		}
		// Land inside the tick window rather than closing immediately.
		time.Sleep(epochTickInterval / 2)
		if err := b.Close(ctx); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
	}
}
