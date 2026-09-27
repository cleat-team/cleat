package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// PluginHealthTracker and RecoverPluginFunc both documented that after a panic,
// future invocations "will return this error without executing the function".
// Nothing enforced it: the closure ended `return fn(ctx, inputJSON)`
// unconditionally, and IsHealthy/UnhealthyError had no callers outside their own
// tests. A plugin panicking on every call was re-entered on every call, forever.
//
// These assert the plugin is NOT REACHED, by counting calls into it. Asserting
// on the returned error would have passed against the old code, which also
// returned a PanicError -- after running the panicking function again.

func TestAPanickedPluginIsNotCalledAgainDuringItsCooldown(t *testing.T) {
	calls := 0
	fn := RecoverPluginFunc("boom", NewPluginHealthTracker(), func(context.Context, string) (string, error) {
		calls++
		panic("bang")
	})

	for i := 0; i < 5; i++ {
		if _, err := fn(context.Background(), "{}"); err == nil {
			t.Fatalf("call %d returned no error from a panicking plugin", i)
		}
	}

	if calls != 1 {
		t.Errorf("the plugin was entered %d times, want 1.\n"+
			"Every call after the first must be refused without reaching it -- that "+
			"refusal is what panic recovery exists to enable, and it was the half "+
			"that was never wired.", calls)
	}
}

// The refusal carries the original PanicError, so a caller learns why rather
// than getting a bare "unavailable".
func TestARefusedCallExplainsTheOriginalPanic(t *testing.T) {
	fn := RecoverPluginFunc("boom", NewPluginHealthTracker(), func(context.Context, string) (string, error) {
		panic("the original cause")
	})

	_, first := fn(context.Background(), "{}")
	_, second := fn(context.Background(), "{}")

	var pe *PanicError
	if !errors.As(second, &pe) {
		t.Fatalf("the refusal is not a PanicError (%v); a caller cannot tell a refused "+
			"call from any other failure", second)
	}
	if !strings.Contains(second.Error(), "the original cause") {
		t.Errorf("refusal %q does not name the panic that caused it", second)
	}
	if first.Error() != second.Error() {
		t.Errorf("the refusal (%q) disagrees with the panic that caused it (%q)", second, first)
	}
}

// THE COOLDOWN IS THE REASON THIS IS SAFE TO WIRE IN. The commonest panic is
// input-triggered, so permanent refusal would let one malformed request disable
// a plugin for every tenant -- with nothing calling MarkHealthy and no HTTP
// surface showing it, that is an unrecoverable, invisible outage a caller can
// trigger deliberately.
func TestThePluginIsTriedAgainAfterTheCooldown(t *testing.T) {
	calls := 0
	failFirst := true
	tracker := NewPluginHealthTrackerWithCooldown(10 * time.Millisecond)
	fn := RecoverPluginFunc("flaky", tracker, func(context.Context, string) (string, error) {
		calls++
		if failFirst {
			panic("bad input")
		}
		return `{"ok":true}`, nil
	})

	if _, err := fn(context.Background(), "{}"); err == nil {
		t.Fatal("the first call did not report the panic")
	}
	if _, err := fn(context.Background(), "{}"); err == nil {
		t.Fatal("a call during the cooldown was not refused")
	}
	if calls != 1 {
		t.Fatalf("plugin entered %d times during the cooldown, want 1", calls)
	}

	failFirst = false
	time.Sleep(15 * time.Millisecond)

	out, err := fn(context.Background(), "{}")
	if err != nil {
		t.Fatalf("the probe after the cooldown was refused: %v -- an input-specific "+
			"panic would then be a permanent outage", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("probe returned %q, want the plugin's real output", out)
	}
	if calls != 2 {
		t.Errorf("plugin entered %d times, want 2 (the panic and the probe)", calls)
	}
}

// A probe that panics again re-arms the refusal, rather than leaving the plugin
// open to be hammered once the first window elapses.
func TestAProbeThatPanicsAgainRefusesAgain(t *testing.T) {
	calls := 0
	tracker := NewPluginHealthTrackerWithCooldown(10 * time.Millisecond)
	fn := RecoverPluginFunc("always", tracker, func(context.Context, string) (string, error) {
		calls++
		panic("still broken")
	})

	fn(context.Background(), "{}")
	time.Sleep(15 * time.Millisecond)
	fn(context.Background(), "{}") // the probe, which panics
	fn(context.Background(), "{}") // must be refused again

	if calls != 2 {
		t.Errorf("plugin entered %d times, want 2 (first panic, one probe). A probe "+
			"that panics must re-arm the refusal, not leave it open.", calls)
	}
}

// One plugin panicking must not refuse another.
func TestRefusalIsPerPlugin(t *testing.T) {
	tracker := NewPluginHealthTracker()
	boom := RecoverPluginFunc("boom", tracker, func(context.Context, string) (string, error) {
		panic("bang")
	})
	fine := RecoverPluginFunc("fine", tracker, func(context.Context, string) (string, error) {
		return "ok", nil
	})

	boom(context.Background(), "{}")
	if out, err := fine(context.Background(), "{}"); err != nil || out != "ok" {
		t.Errorf("a healthy plugin was refused because another panicked: %q %v", out, err)
	}
}

// The streaming wrapper carries the same refusal; a panicking stream provider
// would otherwise be re-entered on every subscribe.
func TestAPanickedStreamProviderIsAlsoRefused(t *testing.T) {
	calls := 0
	fn := RecoverPluginStreamFunc("stream", NewPluginHealthTracker(),
		func(context.Context, string) (<-chan StreamEvent, error) {
			calls++
			panic("setup exploded")
		})

	fn(context.Background(), "{}")
	fn(context.Background(), "{}")

	if calls != 1 {
		t.Errorf("stream provider entered %d times, want 1", calls)
	}
}
