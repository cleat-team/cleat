//go:build ignore

package main

import (
	"testing"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

// A real test, not a placeholder.
//
// An earlier version of this file was a t.Skip, and cleat's own
// scripts/check-skips.sh refused it -- correctly. A skip is indistinguishable
// from a pass, and a scaffold that ships one teaches that habit to every
// project generated from it.
func TestSubmitOrder(t *testing.T) {
	env := cleattest.NewTestEnv()

	// Stub the external call so the workflow runs without network. nil matches
	// any request body; pass a string, or a func(string) bool, to be stricter.
	env.OnCall("http", "fetch", nil).Return(`{"status":200,"body":"ok"}`, nil)

	result, err := SubmitOrder(env.H(), `{"item":"widget","qty":1}`)
	if err != nil {
		t.Fatalf("SubmitOrder failed: %v", err)
	}
	if result != `{"ok":true}` {
		t.Errorf("got %q, want %q", result, `{"ok":true}`)
	}
}
