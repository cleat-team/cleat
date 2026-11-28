package engine

import (
	"context"
	"testing"
)

// cleat#4061. walkToTerminalRun finds the last run of a ContinueAsNew chain and then reads that run's row, as separate statements. If the
// chain moves on in between -- the run it settled on continues as new -- the row it reads is the run it LEFT: status `done`, result `{}`,
// because a run that continues is finalised as done with an empty result. The reply then names an intermediate run as the end of the chain,
// and a client polling for a terminal status takes it for the answer. TestTypeScriptContinueAsNewOnARealWorker saw exactly that on two
// dialects: a `done` run, empty result, not the one it started.
//
// These drive the race deterministically: `successorOf` answers "none" once and then reports the successor, which is what the store does
// when the continue lands between the lookup and the read. No database, no timing.

type chainStep struct {
	id     string
	status string
	result string
}

// scriptedChain is a chain whose successor lookups are answered from a script, so a test can say "the first time you are asked about
// a, there is no successor yet; afterwards there is".
type scriptedChain struct {
	rows       map[string]chainStep
	successors map[string][]string // successive answers per id; the last repeats
	asked      map[string]int
	reads      []string
}

func (c *scriptedChain) successorOf(_ context.Context, id string) (string, error) {
	answers := c.successors[id]
	i := c.asked[id]
	c.asked[id]++
	if len(answers) == 0 {
		return "", nil
	}
	if i >= len(answers) {
		i = len(answers) - 1
	}
	return answers[i], nil
}

func (c *scriptedChain) get(_ context.Context, id string) (*WorkflowInstance, error) {
	c.reads = append(c.reads, id)
	r, ok := c.rows[id]
	if !ok {
		return nil, nil
	}
	return &WorkflowInstance{ID: r.id, Status: r.status, Result: r.result}, nil
}

func newScriptedChain(successors map[string][]string, rows ...chainStep) *scriptedChain {
	c := &scriptedChain{rows: map[string]chainStep{}, successors: successors, asked: map[string]int{}}
	for _, r := range rows {
		c.rows[r.id] = r
	}
	return c
}

func TestAChainThatMovesOnBetweenTheLookupAndTheReadIsFollowedNotReturned(t *testing.T) {
	// a is the end of the chain when the walk asks, and has continued to b by the time its row is read: so a's row is done with an
	// empty result, b is what the chain ends at, and b is running or done with the answer.
	c := newScriptedChain(
		map[string][]string{"a": {"", "b"}, "b": {""}},
		chainStep{"a", "done", "{}"},
		chainStep{"b", "done", `{"n":4}`},
	)

	got, err := walkToTerminalRun(context.Background(), "a", c.successorOf, c.get)
	if err != nil {
		t.Fatalf("walkToTerminalRun: %v", err)
	}
	if got == nil || got.ID != "b" {
		t.Fatalf("the walk returned %+v, want run b.\n\nrun a was the end of the chain when the walk asked and had continued by the time its row was "+
			"read, so its row is status `done` with an empty result -- an intermediate run reported as the end of the chain.", got)
	}
	if got.Result != `{"n":4}` {
		t.Errorf("result = %q, want the answer of the run the chain really ends at", got.Result)
	}
}

// The control the test above needs, so it cannot pass against a walk that always keeps going: with no race the walk returns the run it
// found, and the extra lookup the fix adds is the only cost.
func TestAChainThatDoesNotMoveIsReturnedAsBefore(t *testing.T) {
	c := newScriptedChain(
		map[string][]string{"a": {"b"}, "b": {""}},
		chainStep{"a", "done", "{}"},
		chainStep{"b", "done", `{"n":4}`},
	)
	got, err := walkToTerminalRun(context.Background(), "a", c.successorOf, c.get)
	if err != nil || got == nil || got.ID != "b" {
		t.Fatalf("walkToTerminalRun = %+v, %v; want run b", got, err)
	}

	// A run that never continued is its own terminal run.
	solo := newScriptedChain(map[string][]string{}, chainStep{"s", "running", ""})
	got, err = walkToTerminalRun(context.Background(), "s", solo.successorOf, solo.get)
	if err != nil || got == nil || got.ID != "s" {
		t.Fatalf("a run that never continued: %+v, %v; want itself", got, err)
	}
}

func TestAnUnknownIdIsStillNotFoundAfterTheRecheck(t *testing.T) {
	c := newScriptedChain(map[string][]string{})
	got, err := walkToTerminalRun(context.Background(), "nope", c.successorOf, c.get)
	if err != nil || got != nil {
		t.Fatalf("walkToTerminalRun = %+v, %v; want nil, nil for an id naming nothing", got, err)
	}
}

// A chain that keeps moving on is followed as far as it goes, and a cycle is still reported rather than spun on.
func TestAChainThatMovesOnTwiceIsFollowedToItsEnd(t *testing.T) {
	c := newScriptedChain(
		map[string][]string{"a": {"", "b"}, "b": {"", "c"}, "c": {""}},
		chainStep{"a", "done", "{}"}, chainStep{"b", "done", "{}"}, chainStep{"c", "done", `{"n":3}`},
	)
	got, err := walkToTerminalRun(context.Background(), "a", c.successorOf, c.get)
	if err != nil || got == nil || got.ID != "c" {
		t.Fatalf("walkToTerminalRun = %+v, %v; want run c, the end after two moves", got, err)
	}
}
