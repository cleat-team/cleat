package typescript

// Deferred work for the TypeScript guest (cleat#3826, decision (b) on the issue).
//
// BOTH DIRECTIONS ARE ASSERTED HERE, and in the same file on purpose: an assertion that a defer does NOT run when the
// workflow suspends passes trivially if defers are never wired at all. So the positive arm runs first, and a drain that
// never fires reddens it.
//
// What is deliberately NOT asserted, because it does not happen: a defer running when the HOST kills the workflow. This
// guest exports no drain for the host to call, so the deferred work is lost; that is cleat#3922 and it is stated in
// packages/cleat-ts/DIVERGENCES.md rather than tested as if it worked.
//
// Also not asserted, because it CANNOT be observed today: the drain's `__cleat_suspended()` guard, which mirrors
// cleat-sdk's own rule (a suspended run has not exited). I wrote a workflow that suspends and then throws, so the error
// path reaches the drain while suspended, and removing the guard left the suite GREEN — measured, not assumed, against
// a build that carried that workflow (removed afterwards: with the guard present and absent both green, it could not
// discriminate, and a test that cannot fail is worse than none). The reason is that every effectful binding already
// short-circuits while suspended (the plugin's `guarded` and its `if !is_suspended()` reads), so a drain that wrongly
// ran could only do guest-internal work with no effect to check.
// The guard stays, for parity with Rust and for the day a binding is added that is not short-circuited; the test does
// not claim to cover it.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestTypeScriptDeferredRunsAfterCompletionAndNotOnSuspension(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/deferred.ts"
	res := build(t, root, src, "deferred,deferred_suspends")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}

	// ---- the positive arm: the workflow finishes, so every defer runs ----
	e, closeRT := newEngineWith(t)
	defer closeRT()
	_, _, susp, _, queryState, err := e.Execute(context.Background(), wasm, "deferred", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the completing workflow: %v", err)
	}
	if susp != nil {
		t.Fatalf("the completing workflow suspended (%s)", susp.Reason)
	}
	// The throwing body was registered LAST, so it runs FIRST. That it left a mark at all, AND that the bodies after it
	// still ran, is the isolation assertion: a drain that stops at the first error loses both facts.
	if queryState["threw"] != "yes" {
		t.Fatalf("the throwing defer did not run (query state has %v)", queryState)
	}
	// LIFO, observed through the LAST writer: both bodies write "order", and the one registered FIRST runs LAST.
	if got := queryState["order"]; got != "first" {
		t.Fatalf("query state order = %q, want \"first\" (the first-registered defer must run LAST)", got)
	}

	// ---- the negative arm: the workflow suspends, so NO defer runs ----
	e2, closeRT2 := newEngineWith(t)
	defer closeRT2()
	_, _, susp2, _, queryState2, err := e2.Execute(context.Background(), wasm, "deferred_suspends", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the suspending workflow: %v", err)
	}
	if susp2 == nil {
		t.Fatalf("the second workflow did not suspend, so the negative arm measured nothing")
	}
	if v, ok := queryState2["order"]; ok {
		t.Fatalf("a defer ran for a workflow that only suspended (order=%q); a suspended run has not finished", v)
	}
}
