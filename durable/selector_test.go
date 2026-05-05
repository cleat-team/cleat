package durable

import (
	"strings"
	"testing"
	"time"
)

func TestSelectorSignalWinsImmediately(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep:        func(ms int64) {},
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
	sel.timer.deadline = start.Add(-1 * time.Second)

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
	sel.timer.deadline = time.Unix(0, 0) // way in the past

	winner := sel.Select()
	if winner != SelectorTimer {
		t.Errorf("expected SelectorTimer, got %q", winner)
	}
}

func TestSelectorNilDestinations(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep:        func(ms int64) {},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "sig_a", "payload", false, nil
		},
		PollSignal: func(name string) (string, bool, error) {
			return "payload", true, nil
		},
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	sel.AddSignal("sig_a", nil)   // nil dest — should not panic
	sel.AddTimer(0, nil)           // nil fired — should not panic

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

func TestSelectorAddChildWorkflowReturnsError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 { return 1000 },
	})

	sel := NewSelector(h)
	var result string
	sel.AddChildWorkflow("run_123", &result)

	winner := sel.Select()
	if winner != SelectorError {
		t.Errorf("expected SelectorError, got %q", winner)
	}
	if sel.Err() == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(sel.Err().Error(), "not yet supported") {
		t.Errorf("expected error containing 'not yet supported', got %v", sel.Err())
	}
}
