//go:build ignore

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

// Real tests, not placeholders.
//
// An earlier version of this file was a t.Skip, and cleat's own
// scripts/check-skips.sh refused it -- correctly. A skip is indistinguishable
// from a pass, and a scaffold that ships one teaches that habit to every
// project generated from it.
func TestSubmitOrder(t *testing.T) {
	env := cleattest.NewTestEnv()

	result, err := runOrder(t, env, `{"item":"widget","qty":1}`)
	if err != nil {
		t.Fatalf("SubmitOrder failed: %v", err)
	}
	if result != `{"ok":true}` {
		t.Errorf("got %q, want %q", result, `{"ok":true}`)
	}
}

// A bad order fails the run with a message that says what was wrong.
func TestSubmitOrderRejectsABadOrder(t *testing.T) {
	for _, input := range []string{`{"item":"widget","qty":0}`, `{"qty":1}`, `not json`} {
		env := cleattest.NewTestEnv()
		_, err := runOrder(t, env, input)
		if err == nil || !strings.Contains(err.Error(), "invalid order") {
			t.Errorf("input %q: got err %v, want an \"invalid order\" error", input, err)
		}
	}
}

// runOrder runs SubmitOrder and moves the test environment's simulated clock forward until it returns. The
// workflow's DurableSleepMs waits on that clock, not the wall clock, so without this the sleep never ends
// and the test hangs; with it, the three-second wait costs nothing.
func runOrder(t *testing.T, env *cleattest.TestEnv, input string) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := SubmitOrder(env.H(), input)
		done <- result{out, err}
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			return r.out, r.err
		case <-deadline:
			t.Fatal("SubmitOrder did not return within 10s of real time")
		default:
			env.AdvanceTime(time.Second)
			time.Sleep(time.Millisecond)
		}
	}
}
