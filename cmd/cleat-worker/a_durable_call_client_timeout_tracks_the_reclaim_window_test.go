// a_durable_call_client_timeout_tracks_the_reclaim_window_test.go is cleat#3279's
// own guard: the mistake it was written around is that five HTTP clients in
// setup.go hardcoded `Timeout: 30 * time.Second`, independently of
// minimumReclaimAfter, and none of them had anywhere to record the obligation
// that they had to track it. ROUND 6 (cleat#3258) widened minimumReclaimAfter
// well past 30s and none of the five moved, so a legitimately slow durable
// call started failing at the CLIENT rather than ever reaching the system's
// own reclaim logic.
//
// The fix (setup.go) gave four of the five sites a shared field,
// serviceCallTimeout, derived from durableCallClientTimeout(heartbeat) at
// construction, rather than a literal. This file is the part of the fix that
// stops that from quietly reverting: a value-level invariant, and a
// source-level guard against a bare literal creeping back into any of the
// five call sites -- the fifth (askHolder) in the OPPOSITE direction, which
// is the point cleat#3279's own correction comment makes: "the derivation is
// the right instrument, but the direction and the base differ per site."
package main

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// TestDurableCallClientTimeoutNeverDropsBelowTheReclaimWindow is the
// value-level half: durableCallClientTimeout is defined as
// minimumReclaimAfter today, so this is trivially true by construction --
// its job is to fail the moment that stops being true, the same way
// TestReclaimWindowDefaultMatchesTheStatedInvariant
// (reaper_recovery_grace_period_test.go) guards reclaimWindow against a
// formula computed separately from minimumReclaimAfter.
func TestDurableCallClientTimeoutNeverDropsBelowTheReclaimWindow(t *testing.T) {
	for _, hb := range []time.Duration{
		time.Second, 2 * time.Second, 5 * time.Second,
		30 * time.Second, 60 * time.Second, 149 * time.Second,
	} {
		got := durableCallClientTimeout(hb)
		want := minimumReclaimAfter(hb)
		if got < want {
			t.Errorf("durableCallClientTimeout(%v) = %v, which is BELOW minimumReclaimAfter(%v) = %v -- "+
				"a durable call's own client would time out before the system's reclaim logic "+
				"ever gets consulted, exactly the defect cleat#3279 was filed for", hb, got, hb, want)
		}
	}
}

// TestDurableCallClientSitesDeriveTheirTimeoutRatherThanHardcodingOne is the
// source-level half, and the one that actually catches the mistake class:
// a value-level test on durableCallClientTimeout alone cannot see whether
// any of the five call sites still use a bare literal instead of calling it
// (or, for askHolder, instead of deriving from dbCallDeadline). Four sites
// must read c.serviceCallTimeout; the fifth (askHolder, the reaper's own
// veto call -- part of the reclaim window's mechanism, not a client that
// must survive it) must NOT: it derives from w.dbCallDeadline() instead, in
// the opposite direction. See durableCallClientTimeout's and askHolder's own
// doc comments in setup.go for why the two groups diverge.
//
// A bare numeric literal anywhere in this set -- `Timeout: <N> * time.Second`
// -- is the regression this test exists to catch, whichever of the five
// functions it reappears in.
func TestDurableCallClientSitesDeriveTheirTimeoutRatherThanHardcodingOne(t *testing.T) {
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatalf("UNMEASURED: could not read setup.go to check it: %v", err)
	}
	text := string(src)

	literalTimeout := regexp.MustCompile(`Timeout:\s*\d+\s*\*\s*time\.Second`)

	mustDeriveFromReclaimWindow := []string{
		"forwardToService", "ResolveCall", "ReplayUnderOriginalKey", "handleHTTPFetch",
	}
	for _, fn := range mustDeriveFromReclaimWindow {
		body := funcBody(t, text, fn)
		if !regexp.MustCompile(`Timeout:\s*c\.serviceCallTimeout`).MatchString(body) {
			t.Errorf("%s's http.Client does not set Timeout: c.serviceCallTimeout -- "+
				"it must derive from durableCallClientTimeout(heartbeat) via that field, "+
				"not a literal, or a legitimately slow durable call will fail at the client "+
				"before the system's reclaim logic is ever consulted (cleat#3279)", fn)
		}
		if m := literalTimeout.FindString(body); m != "" {
			t.Errorf("%s's http.Client sets a bare %q -- cleat#3279's whole point is that a "+
				"literal here has nowhere to record its obligation to track minimumReclaimAfter "+
				"and silently stops tracking it; use c.serviceCallTimeout", fn, m)
		}
	}

	askHolderBody := funcBody(t, text, "askHolder")
	if !regexp.MustCompile(`Timeout:\s*2\s*\*\s*w\.dbCallDeadline\(\)`).MatchString(askHolderBody) {
		t.Error("askHolder's http.Client does not derive Timeout from w.dbCallDeadline() -- " +
			"this site is the reaper's own veto call, part of the reclaim window's mechanism " +
			"rather than a client that must survive it, so it must stay above the ask phase's " +
			"own context deadline (one dbCallDeadline), not be raised toward minimumReclaimAfter " +
			"the way the four durable-call sites are (cleat#3279)")
	}
	if m := literalTimeout.FindString(askHolderBody); m != "" {
		t.Errorf("askHolder's http.Client sets a bare %q -- it needs to stay derived from "+
			"w.dbCallDeadline() so it keeps exceeding askCtx's own deadline at every valid "+
			"--heartbeat, not a literal that silently stops doing that above a ~60s heartbeat "+
			"(cleat#3279)", m)
	}
}

// funcBody extracts the text of the named top-level function or method from
// src, from its `func` line up to (but not including) the next top-level
// `func` declaration. Good enough for this file's purpose -- distinguishing
// which of several sibling functions a match fell inside -- without a real
// Go parser.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	// Anchored at column 0: a real declaration, not a reference to the name
	// inside a comment or another function's body.
	start := regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?` + regexp.QuoteMeta(name) + `\(`)
	loc := start.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("could not find a top-level declaration of %s in setup.go -- "+
			"has it been renamed or removed? (cleat#3279's guard needs updating, not deleting)", name)
	}
	rest := src[loc[1]:]
	next := regexp.MustCompile(`(?m)^func\s`).FindStringIndex(rest)
	if next == nil {
		return rest // name is the last function in the file
	}
	return rest[:next[0]]
}
