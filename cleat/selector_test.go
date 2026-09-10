package cleat

import (
	"sync"
	"testing"
	"time"
)

func TestSelectorSignalWinsImmediately(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "sig_a", `{"key":"val"}`, false, nil
		},
		PollSignal: func(name string) (string, bool, error) {
			if name == "sig_a" {
				return `{"key":"val"}`, true, nil
			}
			return "", false, nil
		},
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	var payload string
	var fired bool
	sel.AddSignal("sig_a", &payload)
	sel.AddTimer(time.Hour, &fired)

	winner := sel.Select()
	if winner != "sig_a" {
		t.Errorf("expected 'sig_a', got %q", winner)
	}
	if payload != `{"key":"val"}` {
		t.Errorf("expected payload, got %q", payload)
	}
	if fired {
		t.Error("timer should not have fired")
	}
}

func TestSelectorTimerWins(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil // always timeout
		},
		PollSignal: func(name string) (string, bool, error) {
			return "", false, nil
		},
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	var payload string
	var fired bool
	sel.AddSignal("sig_a", &payload)
	sel.AddTimer(0, &fired) // immediate timer

	winner := sel.Select()
	if winner != SelectorTimer {
		t.Errorf("expected SelectorTimer, got %q", winner)
	}
	if !fired {
		t.Error("timer should have fired")
	}
}

func TestSelectorTimerFiresWhenNowPassesDeadline(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil
		},
		PollSignal: func(name string) (string, bool, error) {
			return "", false, nil
		},
		Now: func() int64 { return start.UnixMilli() },
	})

	sel := NewSelector(h)
	var payload string
	var fired bool
	sel.AddSignal("sig_a", &payload)
	sel.AddTimer(1*time.Second, &fired)

	// Force timer deadline to be in the past by recreating with a past deadline.
	sel.timers[0].deadline = start.Add(-1 * time.Second)

	winner := sel.Select()
	if winner != SelectorTimer {
		t.Errorf("expected SelectorTimer, got %q", winner)
	}
}

func TestSelectorMultipleSignals(t *testing.T) {
	signalCh := make(chan string, 1)
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			// Return the first name in the list
			return names[0], "payload_" + names[0], false, nil
		},
		PollSignal: func(name string) (string, bool, error) {
			select {
			case sig := <-signalCh:
				if sig == name {
					return "payload", true, nil
				}
				return "", false, nil
			default:
				return "", false, nil
			}
		},
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	var p1, p2 string
	sel.AddSignal("driver_accepted", &p1)
	sel.AddSignal("order_confirmed", &p2)

	winner := sel.Select()
	if winner != "driver_accepted" {
		t.Errorf("expected 'driver_accepted', got %q", winner)
	}
	if p1 != "payload_driver_accepted" {
		t.Errorf("expected payload, got %q", p1)
	}
}

func TestSelectorNoFutures(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	winner := sel.Select()
	if winner != "" {
		t.Errorf("expected empty string, got %q", winner)
	}
}

func TestSelectorTimerOnly(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		Now:          func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	var fired bool
	sel.AddTimer(0, &fired)
	sel.timers[0].deadline = time.Unix(0, 0) // way in the past

	winner := sel.Select()
	if winner != SelectorTimer {
		t.Errorf("expected SelectorTimer, got %q", winner)
	}
}

func TestSelectorNilDestinations(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "sig_a", "payload", false, nil
		},
		PollSignal: func(name string) (string, bool, error) {
			return "payload", true, nil
		},
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	sel.AddSignal("sig_a", nil) // nil dest — should not panic
	sel.AddTimer(0, nil)        // nil fired — should not panic

	// Should not panic.
	winner := sel.Select()
	if winner != "sig_a" {
		t.Errorf("expected 'sig_a', got %q", winner)
	}
}

func TestSelectorAddChildWorkflow(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	var result string
	sel.AddChildWorkflow("run_123", &result)

	if len(sel.children) != 1 {
		t.Fatalf("expected 1 child future, got %d", len(sel.children))
	}
	if sel.children[0].runID != "run_123" {
		t.Errorf("expected runID 'run_123', got %q", sel.children[0].runID)
	}
}

func TestSelectorAddChildWorkflowCompletes(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 { return 1000 },
		AwaitChild: func(runID string) (string, error) {
			return `{"status":"done"}`, nil
		},
	})

	sel := NewSelector(h)
	var result string
	sel.AddChildWorkflow("run_123", &result)

	winner := sel.Select()
	if winner != "run_123" {
		t.Errorf("expected 'run_123', got %q", winner)
	}
	if result != `{"status":"done"}` {
		t.Errorf("expected result %q, got %q", `{"status":"done"}`, result)
	}
	if sel.Err() != nil {
		t.Errorf("expected nil error, got %v", sel.Err())
	}
}

func TestSelectorChildWithSignals(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 { return 1000 },
		PollSignal: func(signalName string) (string, bool, error) {
			return "", false, nil
		},
		AwaitChild: func(runID string) (string, error) {
			if runID == "child_done" {
				return `{"status":"done"}`, nil
			}
			return "", nil // child not done — loop continues
		},
	})

	sel := NewSelector(h)
	var childResult string
	var sigResult string
	sel.AddChildWorkflow("child_done", &childResult)
	sel.AddSignal("some_signal", &sigResult)

	winner := sel.Select()
	// Child should win since it completes immediately.
	if winner != "child_done" {
		t.Errorf("expected 'child_done', got %q", winner)
	}
	if childResult != `{"status":"done"}` {
		t.Errorf("expected child result, got %q", childResult)
	}
}

// ---------------------------------------------------------------------------
// Concurrency and race-condition tests
// ---------------------------------------------------------------------------

func TestSelectorConcurrentSignalAndTimer(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	nowMs := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	signalDelivered := false

	// Channels allow AwaitSignals to receive either a signal result or a
	// timeout event, simulating the race between the two.
	sigResultCh := make(chan struct{ name, payload string }, 1)
	timeoutCh := make(chan struct{}, 1)

	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			mu.Lock()
			delivered := signalDelivered
			mu.Unlock()
			if delivered && name == "order_shipped" {
				return "shipped-payload", true, nil
			}
			return "", false, nil
		},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			select {
			case sig := <-sigResultCh:
				return sig.name, sig.payload, false, nil
			case <-timeoutCh:
				return "", "", true, nil
			}
		},
		DurableSleep: func(ms int64) {},
		Now: func() int64 {
			mu.Lock()
			defer mu.Unlock()
			return nowMs
		},
	})

	sel := NewSelector(h)
	var payload string
	var timerFired bool
	sel.AddSignal("order_shipped", &payload)
	sel.AddTimer(100*time.Millisecond, &timerFired)

	resultCh := make(chan string, 1)
	go func() {
		resultCh <- sel.Select()
	}()

	// Give the selector time to enter the loop and call AwaitSignals.
	time.Sleep(10 * time.Millisecond)

	// Race: deliver signal AND advance time past the timer deadline
	// simultaneously, so both outcomes are possible when AwaitSignals
	// decides which to return.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		mu.Lock()
		signalDelivered = true
		mu.Unlock()
		sigResultCh <- struct{ name, payload string }{"order_shipped", "shipped-payload"}
	}()

	go func() {
		defer wg.Done()
		mu.Lock()
		nowMs += 200 // advance past 100ms timer deadline
		mu.Unlock()
		timeoutCh <- struct{}{}
	}()

	wg.Wait()

	select {
	case winner := <-resultCh:
		t.Logf("Winner: %q (payload=%q, timerFired=%v)", winner, payload, timerFired)
		if winner != "order_shipped" && winner != SelectorTimer {
			t.Errorf("expected 'order_shipped' or SelectorTimer, got %q", winner)
		}
		if payload != "" && timerFired {
			t.Error("both signal payload and timer fired are set — duplicate dispatch detected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("test timed out — Select() did not return")
	}
}

func TestSelectorMultipleConcurrentSignals(t *testing.T) {
	t.Parallel()

	sigACh := make(chan struct{ payload string }, 1)
	sigBCh := make(chan struct{ payload string }, 1)
	sigCCh := make(chan struct{ payload string }, 1)

	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			var ch chan struct{ payload string }
			switch name {
			case "sig_a":
				ch = sigACh
			case "sig_b":
				ch = sigBCh
			case "sig_c":
				ch = sigCCh
			default:
				return "", false, nil
			}
			select {
			case sig := <-ch:
				return sig.payload, true, nil
			default:
				return "", false, nil
			}
		},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			select {
			case sig := <-sigACh:
				return "sig_a", sig.payload, false, nil
			case sig := <-sigBCh:
				return "sig_b", sig.payload, false, nil
			case sig := <-sigCCh:
				return "sig_c", sig.payload, false, nil
			case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
				return "", "", true, nil
			}
		},
		DurableSleep: func(ms int64) {},
		Now: func() int64 {
			return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		},
	})

	sel := NewSelector(h)
	var pA, pB, pC string
	sel.AddSignal("sig_a", &pA)
	sel.AddSignal("sig_b", &pB)
	sel.AddSignal("sig_c", &pC)

	resultCh := make(chan string, 1)
	go func() {
		resultCh <- sel.Select()
	}()

	// Give the selector time to enter AwaitSignals.
	time.Sleep(10 * time.Millisecond)

	// Fire all 3 signals simultaneously from 3 goroutines.
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		sigACh <- struct{ payload string }{"payload_a"}
	}()
	go func() {
		defer wg.Done()
		sigBCh <- struct{ payload string }{"payload_b"}
	}()
	go func() {
		defer wg.Done()
		sigCCh <- struct{ payload string }{"payload_c"}
	}()

	wg.Wait()

	select {
	case winner := <-resultCh:
		t.Logf("Winner: %q (pA=%q, pB=%q, pC=%q)", winner, pA, pB, pC)

		if winner != "sig_a" && winner != "sig_b" && winner != "sig_c" {
			t.Errorf("expected one of sig_a/sig_b/sig_c, got %q", winner)
		}
		// Exactly one signal destination should be populated.
		count := 0
		if pA != "" {
			count++
		}
		if pB != "" {
			count++
		}
		if pC != "" {
			count++
		}
		if count != 1 {
			t.Errorf("expected exactly 1 signal destination populated, got %d", count)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("test timed out — Select() did not return")
	}
}

func TestSelectorSignalDuringReplay(t *testing.T) {
	t.Parallel()

	signalName := "my_signal"
	signalPayload := "deterministic-payload"

	// ---- Record phase ----
	// AwaitSignals blocks until an external signal is delivered.
	recordSignalCh := make(chan struct{}, 1)

	hRecord := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			return "", false, nil
		},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			// Block until signal arrives or the internal timeout fires.
			select {
			case <-recordSignalCh:
				return signalName, signalPayload, false, nil
			case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
				return "", "", true, nil
			}
		},
		DurableSleep: func(ms int64) {},
		Now: func() int64 {
			return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		},
	})

	var recPayload string
	selRecord := NewSelector(hRecord)
	selRecord.AddSignal(signalName, &recPayload)

	recordResultCh := make(chan string, 1)
	go func() {
		recordResultCh <- selRecord.Select()
	}()

	// Give the selector time to enter AwaitSignals.
	time.Sleep(5 * time.Millisecond)

	// Deliver the signal (simulating an external event during execution).
	recordSignalCh <- struct{}{}

	select {
	case recordWinner := <-recordResultCh:
		t.Logf("Record phase: winner=%q, payload=%q", recordWinner, recPayload)
		if recordWinner != signalName {
			t.Fatalf("record expected winner %q, got %q", signalName, recordWinner)
		}
		if recPayload != signalPayload {
			t.Fatalf("record expected payload %q, got %q", signalPayload, recPayload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("record phase timed out")
	}

	// ---- Replay phase ----
	// AwaitSignals returns immediately with the cached result (no blocking).
	hReplay := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			return "", false, nil
		},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			// Return the same result as the record phase, immediately.
			return signalName, signalPayload, false, nil
		},
		DurableSleep: func(ms int64) {},
		Now: func() int64 {
			return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		},
	})

	var replayPayload string
	selReplay := NewSelector(hReplay)
	selReplay.AddSignal(signalName, &replayPayload)

	replayWinner := selReplay.Select()
	t.Logf("Replay phase: winner=%q, payload=%q", replayWinner, replayPayload)

	// Replay must produce the same result as the original execution.
	if replayWinner != signalName {
		t.Errorf("replay expected winner %q, got %q", signalName, replayWinner)
	}
	if replayPayload != signalPayload {
		t.Errorf("replay expected payload %q, got %q", signalPayload, replayPayload)
	}
}

// cleat#1129. AddTimer assigned rather than appended, so a second call
// silently discarded the first — no error, no log, nothing at build time.
// AddSignal and AddChildWorkflow both accumulate; only this one did not, and
// the asymmetry was unannounced.
//
// The neighbouring limitation is what makes the silence expensive:
// AddChildWorkflow's restriction is documented ten lines above AddTimer AND
// fails loudly through Select. A caller who read that comment learned the
// package announces its limitations, which is exactly the wrong lesson to
// carry to the call below it.
func TestSelectorRacesTwoTimersInsteadOfDiscardingOne(t *testing.T) {
	now := int64(1000)
	slept := []int64{}
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) { slept = append(slept, ms); now += ms },
		PollSignal:   func(name string) (string, bool, error) { return "", false, nil },
		Now:          func() int64 { return now },
	})

	sel := NewSelector(h)
	var soft, hard bool
	sel.AddTimer(30*time.Second, &soft) // the earlier deadline
	sel.AddTimer(5*time.Minute, &hard)  // must not discard the one above

	winner := sel.Select()

	if winner != SelectorTimer {
		t.Fatalf("Select returned %q, want %q", winner, SelectorTimer)
	}
	if !soft {
		t.Error("the 30s timer never fired.\n\n" +
			"Two timers were added and the earlier one is the one that should " +
			"win a race. Before cleat#1129 the second AddTimer overwrote the " +
			"first, so `soft` could never be set and the workflow waited five " +
			"minutes for a deadline it had asked to be thirty seconds.")
	}
	if hard {
		t.Error("the 5m timer fired too; only the winning deadline should be " +
			"marked, or a caller cannot tell which deadline it was woken for")
	}
	// The sleep must be to the EARLIEST deadline, not the last one added.
	if len(slept) != 1 || slept[0] != 30_000 {
		t.Errorf("slept %v ms, want exactly [30000]. Sleeping to the later "+
			"deadline is the same defect wearing a different symptom: the "+
			"earlier timer eventually fires, four and a half minutes late.", slept)
	}
}

// The ordering must not depend on the order the timers were added: a caller
// racing a long deadline against a short one writes them in whichever order
// reads best, and the earliest is still the one that fires.
func TestSelectorEarliestTimerWinsRegardlessOfAddOrder(t *testing.T) {
	now := int64(1000)
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) { now += ms },
		PollSignal:   func(name string) (string, bool, error) { return "", false, nil },
		Now:          func() int64 { return now },
	})

	sel := NewSelector(h)
	var late, early bool
	sel.AddTimer(5*time.Minute, &late)   // added first
	sel.AddTimer(30*time.Second, &early) // earlier deadline, added second

	if winner := sel.Select(); winner != SelectorTimer {
		t.Fatalf("Select returned %q, want %q", winner, SelectorTimer)
	}
	if !early {
		t.Error("the 30s timer did not fire though it has the earliest deadline")
	}
	if late {
		t.Error("the 5m timer fired; add order decided the race rather than the deadline")
	}
}

// A single AddTimer must behave exactly as before. This is the compatibility
// half: the fix is only safe because nobody can depend on the discard, and
// every existing caller in the tree passes one timer.
func TestSelectorSingleTimerIsUnchanged(t *testing.T) {
	now := int64(1000)
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) { now += ms },
		PollSignal:   func(name string) (string, bool, error) { return "", false, nil },
		Now:          func() int64 { return now },
	})

	sel := NewSelector(h)
	var fired bool
	sel.AddTimer(0, &fired)

	if winner := sel.Select(); winner != SelectorTimer {
		t.Fatalf("Select returned %q, want %q", winner, SelectorTimer)
	}
	if !fired {
		t.Error("a lone immediate timer did not fire")
	}
}

// The third site cleat#1129 touched, and the one my first pass left unguarded.
//
// When signals are present, Select does not sleep — it calls AwaitSignals with
// a timeout capped at the nearest deadline. That computation reads the timer
// list too, and it is invisible to the tests above because none of them adds a
// signal. Mutating it to use the last-added timer left the whole selector
// suite green, which is how this test came to exist: falsify each changed site
// separately, because "the change is covered" is not a per-site claim.
func TestSelectorAwaitSignalsTimeoutIsTheEarliestDeadline(t *testing.T) {
	now := int64(1000)
	var gotTimeouts []int64
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) { now += ms },
		PollSignal:   func(name string) (string, bool, error) { return "", false, nil },
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			gotTimeouts = append(gotTimeouts, timeoutMs)
			now += timeoutMs // the wait actually elapses, so the loop terminates
			return "", "", true, nil
		},
		Now: func() int64 { return now },
	})

	sel := NewSelector(h)
	var payload string
	var soft, hard bool
	sel.AddSignal("sig_a", &payload)
	sel.AddTimer(30*time.Second, &soft)
	sel.AddTimer(5*time.Minute, &hard)

	if winner := sel.Select(); winner != SelectorTimer {
		t.Fatalf("Select returned %q, want %q", winner, SelectorTimer)
	}
	if len(gotTimeouts) == 0 {
		t.Fatal("AwaitSignals was never called; this test asserts nothing about " +
			"the timeout computation it exists to cover")
	}
	if gotTimeouts[0] != 30_000 {
		t.Errorf("AwaitSignals was given a %d ms timeout, want 30000.\n\n"+
			"With two deadlines pending the wait must be capped at the EARLIEST, "+
			"or the signal wait outlives the deadline it was supposed to race "+
			"and the 30s timer fires four and a half minutes late.", gotTimeouts[0])
	}
	if !soft {
		t.Error("the 30s timer did not fire")
	}
	if hard {
		t.Error("the 5m timer fired; the earlier deadline should have won")
	}
}
