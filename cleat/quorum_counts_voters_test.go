package cleat

import (
	"strings"
	"testing"
	"time"
)

// cleat#1132: AwaitSignalsWithQuorum counted DELIVERIES, so three copies of one
// name satisfied a quorum of three with the other two never sent.
//
// The fallback loop is the only implementation any production caller reaches:
// the delegating branch is fed from opts.AwaitSignalsWithQuorum, and the sole
// assignment to that option in the tree is in runtime_test.go. It is a test
// seam, not an alternative backend.
//
// These drive DurableAwaitSignals directly, which is what the fallback calls,
// so they exercise the loop rather than the seam.

// deliver returns a DurableAwaitSignals stub that hands back the given names in
// order, and reports a timeout once they run out.
//
// It records the name set it was ASKED for on each call, which is the thing the
// defect was about: `remaining` was passed unchanged every time.
func deliver(names []string, asked *[][]string) func([]string, int64) (string, string, bool, error) {
	i := 0
	return func(want []string, _ int64) (string, string, bool, error) {
		*asked = append(*asked, append([]string(nil), want...))
		if i >= len(names) {
			return "", "", true, nil // timed out
		}
		n := names[i]
		i++
		return n, `{"ok":true}`, false, nil
	}
}

func TestAQuorumIsNotSatisfiedByOneNameVotingRepeatedly(t *testing.T) {
	var asked [][]string
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: deliver([]string{"alpha", "alpha", "alpha"}, &asked),
	})

	got, err := h.AwaitSignalsWithQuorum([]string{"alpha", "beta", "gamma"}, 3, -1, time.Second)
	if err == nil {
		var names []string
		for _, r := range got {
			names = append(names, r.Name)
		}
		t.Fatalf("three deliveries of one name satisfied a quorum of three: %v\n\n"+
			"A quorum counts VOTERS. `maxRejections` makes this a voting primitive, "+
			"and a vote in which one voter votes three times is not the thing anyone "+
			"reaches for it to build (cleat#1132).", names)
	}

	// The mechanism, asserted directly: the set must shrink after alpha votes.
	// Without this the test would also pass against an implementation that
	// failed for some unrelated reason.
	if len(asked) < 2 {
		t.Fatalf("the loop awaited only %d time(s); it cannot have narrowed anything", len(asked))
	}
	first, second := asked[0], asked[1]
	if len(second) >= len(first) {
		t.Errorf("the awaited set did not narrow: first=%v second=%v\n\n"+
			"`remaining` was assigned once from signalNames and passed unchanged to "+
			"every await -- the variable was named for a narrowing that never landed.",
			first, second)
	}
	for _, n := range second {
		if n == "alpha" {
			t.Errorf("alpha was still awaited after voting: %v", second)
		}
	}
}

func TestAQuorumIsSatisfiedByDistinctNames(t *testing.T) {
	// The partner. Without it, "three copies do not satisfy" is also true of an
	// implementation that never satisfies a quorum at all.
	var asked [][]string
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: deliver([]string{"beta", "gamma", "alpha"}, &asked),
	})

	got, err := h.AwaitSignalsWithQuorum([]string{"alpha", "beta", "gamma"}, 3, -1, time.Second)
	if err != nil {
		t.Fatalf("three distinct names did not satisfy a quorum of three: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.Name] {
			t.Errorf("name %q counted twice: %v", r.Name, got)
		}
		seen[r.Name] = true
	}
}

func TestAQuorumLargerThanItsNameSetIsRefusedRatherThanTimingOut(t *testing.T) {
	// Arithmetic forbids it, so it is a caller error rather than a slow sender.
	// It previously spun to the timeout and reported "got 0/4 signals", which
	// describes a sender that never arrived.
	var asked [][]string
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: deliver(nil, &asked),
	})

	_, err := h.AwaitSignalsWithQuorum([]string{"alpha", "beta"}, 4, -1, time.Hour)
	if err == nil {
		t.Fatal("a quorum of 4 over 2 names was accepted")
	}
	if !strings.Contains(err.Error(), "unsatisfiable") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("it awaited %d time(s) before refusing; the refusal should be "+
			"arithmetic, not a timeout", len(asked))
	}
}

func TestNarrowingDoesNotEditTheCallersSlice(t *testing.T) {
	var asked [][]string
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: deliver([]string{"beta", "alpha"}, &asked),
	})

	names := []string{"alpha", "beta", "gamma"}
	if _, err := h.AwaitSignalsWithQuorum(names, 2, -1, time.Second); err != nil {
		t.Fatalf("quorum of 2 over 3 distinct names failed: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("the caller's slice was edited: %v, want %v\n\n"+
				"remaining := signalNames aliased it; narrowing in place would "+
				"rewrite a slice the workflow may still hold.", names, want)
		}
	}
}

// faithfulDeliver models the engine's own DurableAwaitSignals: it returns a
// queued name only if that name is still among the ones being awaited, and
// otherwise reports a timeout, which is what a host does when nothing in the
// requested set arrives.
//
// The unfaithful stub above returns whatever is queued, which exercises the
// out-of-set guard. This one exercises the NARROWING, and the two are different
// mechanisms: with a polite host the repeated name is never delivered a second
// time, and with an impolite one it is delivered and refused. A fix resting on
// only one of those is a fix resting on the host's manners.
func faithfulDeliver(queue []string, asked *[][]string) func([]string, int64) (string, string, bool, error) {
	i := 0
	return func(want []string, _ int64) (string, string, bool, error) {
		*asked = append(*asked, append([]string(nil), want...))
		for i < len(queue) {
			n := queue[i]
			i++
			if containsName(want, n) {
				return n, `{"ok":true}`, false, nil
			}
			// Sent, but nobody is waiting for that name any more: the engine
			// would not hand it to this await.
		}
		return "", "", true, nil
	}
}

func TestAgainstAPoliteHostTheRepeatedNameIsSimplyNeverDeliveredAgain(t *testing.T) {
	var asked [][]string
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: faithfulDeliver([]string{"alpha", "alpha", "alpha"}, &asked),
	})

	_, err := h.AwaitSignalsWithQuorum([]string{"alpha", "beta", "gamma"}, 3, -1, time.Second)
	if err == nil {
		t.Fatal("three copies of one name satisfied a quorum of three against a host " +
			"that only delivers requested names")
	}
	// It must fail as a TIMEOUT -- the quorum was never reached because two
	// voters never voted -- rather than through the out-of-set guard, which
	// would mean the narrowing had not happened.
	if !strings.Contains(err.Error(), "quorum timeout") {
		t.Errorf("failed for the wrong reason: %v\n\n"+
			"Against a polite host the second alpha is never delivered, because "+
			"alpha is no longer awaited. Reaching the out-of-set guard here would "+
			"mean the set had not narrowed.", err)
	}
}
