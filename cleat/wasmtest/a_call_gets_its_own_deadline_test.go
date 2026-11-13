package wasmtest

import (
	"testing"
	"time"
)

// cleat#3746. The env used to carry ONE 30s deadline for its whole life, shared by
// every Execute and Replay: a test with two heavy calls (a 19 MB Python component
// is recompiled on each, cleat#3748) spent it twice, and on a slow runner the
// second call ran with none left. Each call now has its own.

func TestTheEnvLifetimeContextCarriesNoDeadline(t *testing.T) {
	env := NewWasmTestEnv(t)
	defer env.Close()
	if d, ok := env.ctx.Deadline(); ok {
		t.Fatalf("the env's own ctx has a deadline (%v): every call would share it", d)
	}
}

func TestEachCallGetsItsOwnDeadline(t *testing.T) {
	env := NewWasmTestEnv(t, WithCallTimeout(300*time.Millisecond))
	defer env.Close()

	first, cancel1 := env.callCtx()
	defer cancel1()
	d1, ok := first.Deadline()
	if !ok {
		t.Fatal("a call has no deadline, so a hung guest would never fail")
	}
	if left := time.Until(d1); left > 300*time.Millisecond {
		t.Errorf("first call has %v, want at most its 300ms bound", left)
	}

	// Let the first call's whole budget go by, as a slow first call does.
	time.Sleep(400 * time.Millisecond)
	if first.Err() == nil {
		t.Fatal("test setup: the first call's deadline should have passed")
	}

	second, cancel2 := env.callCtx()
	defer cancel2()
	if err := second.Err(); err != nil {
		t.Fatalf("the second call starts with a spent context (%v): it shares the first call's budget", err)
	}
	if left := time.Until(mustDeadline(t, second)); left < 200*time.Millisecond {
		t.Errorf("the second call has only %v of its 300ms bound", left)
	}
}

func TestACallWithNoBoundSetGetsTheDefault(t *testing.T) {
	env := NewWasmTestEnv(t)
	defer env.Close()
	c, cancel := env.callCtx()
	defer cancel()
	left := time.Until(mustDeadline(t, c))
	if left > DefaultCallTimeout || left < DefaultCallTimeout-5*time.Second {
		t.Errorf("default call bound = %v, want about %v", left, DefaultCallTimeout)
	}
}

func mustDeadline(t *testing.T, c interface{ Deadline() (time.Time, bool) }) time.Time {
	t.Helper()
	d, ok := c.Deadline()
	if !ok {
		t.Fatal("no deadline")
	}
	return d
}
