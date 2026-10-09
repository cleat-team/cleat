package main

import (
	"fmt"
	"testing"
	"time"
)

// TestReaperRecoveryGateHoldsAcrossTheParameterSpaceMultiAttempt is cleat#3254,
// the residual of cleat#2175 that
// TestReaperRecoveryGateHoldsAcrossTheParameterSpace's own file doc comment
// (a_reaper_recovery_gate_exhaustive_sweep_test.go) named and declined to
// build: that test sweeps stall length D only over [0, heartbeat), because
// a stall lasting one to two heartbeats needs a multi-attempt state machine
// -- which retry, delayed by which prior failure, first lands after the
// stall clears -- that the existing four-term formula restatement does not
// claim to model. This file is that simulation, covering D over
// [heartbeat, 2*heartbeat], the band cleat#2175's issue body asked for and
// the sibling file explicitly left open.
//
// WHY A SIMULATION AND NOT A WIDER FORMULA. minimumReclaimAfter's own
// closed-form sum (setup.go) has exactly one retry built into it: T1 (wait
// for the next scheduled attempt), T2 (that attempt's own duration, bounded
// at `heartbeat` because D < heartbeat there), T3 (the wait before a
// retry), T4 (the retry's own worst-case latency). Once D can exceed
// heartbeat, the FIRST retry can itself land inside the still-ongoing
// stall and fail too, needing a second retry, and so on -- how many
// attempts land inside the stall depends on the driver's own failure
// shape (how quickly each failing call returns) and is not a fixed count.
// Restating that as one more closed-form term would be inventing an
// unstated extension of the invariant that nobody has reviewed; this
// walks heartbeatLoop's own re-arm rule (setup.go: success -> wait
// heartbeat, failure -> wait heartbeatRetryIntervalFor) attempt by attempt
// instead, the same way the sibling file's own doc explains this gap
// should eventually be closed.
//
// EVERYTHING THIS FILE CARRIES OVER FROM THE SIBLING FILE, UNCHANGED, AND
// WHY: the three driver failure shapes (mysql/postgres/mssql), the
// worst-case "stall begins exactly when the first caught attempt starts
// touching the network" framing, dL and D overlapping rather than
// stacking, the reconnect-latency dimension capped at reclaimSlack's own
// documented value and read as a pin inside that budget rather than a
// measurement of real reconnect cost, and the retry's own worst-case
// latency being modeled as `deadline` (not the swept dL) even once the
// stall has genuinely cleared -- because minimumReclaimAfter's own T4 term
// is `deadline`, not an ordinary round trip, and understating it here
// would pass a grid point the real invariant does not actually cover.
// Changing any of those here would silently test a DIFFERENT model than
// the one this file is supposed to extend. See
// a_reaper_recovery_gate_exhaustive_sweep_test.go's doc comment for the
// full justification of each; this file states only what it reuses and
// what is new.
//
// WHAT IS GENUINELY NEW: every attempt after the first that fails in mssql
// mode pays the reconnect cost too, not only the first retry -- go-mssqldb's
// own checkBadConn fires on every failed drain, not a special case for the
// first one, so a stall spanning several failed attempts pays reconnect
// before each one after the first. The sibling file never needed this
// because it never has more than one failed attempt to begin with.
//
// THE RESULT: minimumReclaimAfter DOES NOT HOLD ACROSS THIS WHOLE BAND, for
// any of the three modes -- filed as cleat#3258, not decided here. This
// file's own assertions are built to make that a CHECKED claim rather than
// a comment: postgres gets an exact, closed-form safe/unsafe boundary
// (derived below, not borrowed from the simulation) because its call never
// returns early, so it always takes exactly one failed attempt regardless
// of D, with no multi-attempt branching to characterize. mysql and mssql
// need a genuine attempt count with a partial final round, and a second
// closed form for that would either duplicate simulateRecovery's own
// branching (not meaningfully independent) or risk a fresh off-by-one at
// real cost -- so those two are characterized by a pinned, re-derivable
// COUNT of currently-unsafe grid points instead (see unsafeMySQL/unsafeMSSQL
// below). Both forms are deliberately written so a future fix to
// minimumReclaimAfter or reclaimSlack (cleat#3258) makes some of today's
// "known gap" points newly safe -- which flips this file's own assertions
// and is the intended signal to revisit it, not a flake.
//
// SCOPE, CARRIED OVER UNCHANGED FROM THE SIBLING FILE: this covers the
// SAFETY direction only ("no live run is reclaimed too early"), not
// liveness, and has no OBSERVER -- same two gaps, same reasons, see that
// file's own doc comment for both. Also unchanged: a stalled dial itself
// (the reconnect) is modeled as a fixed cost, not as something the ongoing
// network stall could also catch -- the sibling file's reconnect dimension
// already carries that same simplification, stated there as "a pin inside
// the slack budget, not evidence about real reconnect latency." Widening
// that is a separate, different piece of work, not inherited here by
// extending D.
func TestReaperRecoveryGateHoldsAcrossTheParameterSpaceMultiAttempt(t *testing.T) {
	// mssqlDrainSlack: same source and same value as the sibling file's own
	// constant of this name -- go-mssqldb's cancel-drain path, cited there
	// as "~5s" in minimumReclaimAfter's own doc comment in setup.go. Not
	// shared via an exported symbol because the sibling file's own copy is
	// a local const inside its test function, not a package-level one.
	const mssqlDrainSlack = 5 * time.Second

	// pinnedReclaimSlack freezes reclaimSlack's value as measured when this
	// file was written (cleat#3254/cleat#3258), deliberately NOT read live
	// from setup.go's reclaimSlack constant. postgresMaxSafeD below is a
	// claim about TODAY's known safe envelope; reading reclaimSlack live
	// would make that claim silently track any future widening of it,
	// which defeats the point -- a fix to reclaimSlack (cleat#3258) should
	// make this file's postgres assertion start failing, not stay quiet.
	// Re-derive by reading reclaimSlack's current value directly in
	// setup.go (`grep -n 'const reclaimSlack' cmd/cleat-worker/setup.go`).
	const pinnedReclaimSlack = 1 * time.Second

	type mode struct {
		name string
		// callDuration: how long a call caught by `remaining` (the stall
		// time still left when this call starts touching the network)
		// takes to return, given ordinary round-trip latency dL. Identical
		// shape to the sibling file's mode.callDuration -- see its doc for
		// why raw duration is max(remaining, dL), not a sum.
		callDuration func(remaining, dL, deadline time.Duration) time.Duration
		// retryPaysReconnect: every attempt after a failed one pays a fresh
		// reconnect before it can start (not just the first retry -- see
		// the file doc comment above).
		retryPaysReconnect bool
		// exactSafeBoundary, when non-nil, is a closed-form (not derived
		// from simulateRecovery) upper bound on D for which this mode is
		// safe -- see the file doc comment on why only postgres gets one.
		exactSafeBoundary func(deadline time.Duration) time.Duration
	}
	rawDuration := func(remaining, dL time.Duration) time.Duration { return max(remaining, dL) }
	modes := []mode{
		{
			name: "mysql (respects ctx, cut off at deadline)",
			callDuration: func(remaining, dL, deadline time.Duration) time.Duration {
				return min(rawDuration(remaining, dL), deadline)
			},
		},
		{
			name: "postgres (ignores ctx, returns when the stall clears)",
			callDuration: func(remaining, dL, _ time.Duration) time.Duration {
				return rawDuration(remaining, dL)
			},
			// postgres never returns early (no ceiling), so a caught call
			// always blocks for exactly `remaining` -- there is never a
			// second failed attempt, by construction, regardless of D's
			// size. recovery(D) = heartbeat + D + retryInterval + deadline
			// (dL never binds: D >= heartbeat always on this file's own
			// grid, and dL <= deadline always by construction (dL is
			// swept over [0, deadline]), so max(D, dL) = D). This does
			// NOT need heartbeat > deadline -- an earlier version of this
			// comment claimed that too, which is false at the "low" scale
			// (dbCallDeadlineFor(1s) = 2s, the floor binds, so deadline >
			// heartbeat there). Caught by cleat-review during #3259's
			// FINAL verification (cleat#3262): the assertion below was
			// never affected, since it only ever needed dL <= deadline.
			// Solving
			// recovery(D) <= minimumReclaimAfter(heartbeat) for D gives
			// exactly 2*deadline + pinnedReclaimSlack -- pure algebra on
			// minimumReclaimAfter's own stated formula
			// (heartbeat + 3*deadline + retryInterval + reclaimSlack),
			// not a value read off the simulation.
			exactSafeBoundary: func(deadline time.Duration) time.Duration {
				return 2*deadline + pinnedReclaimSlack
			},
		},
		{
			name: "mssql (cancel-drain bound, then a reconnect on every retry)",
			callDuration: func(remaining, dL, deadline time.Duration) time.Duration {
				return min(rawDuration(remaining, dL), deadline+mssqlDrainSlack)
			},
			retryPaysReconnect: true,
		},
	}

	type hbScale struct {
		name string
		hb   time.Duration
	}
	// Same three scales as the sibling file, for the same reason: real
	// production-scale durations, since this sweep is pure arithmetic and
	// costs no wall-clock time regardless of the values chosen.
	hbScales := []hbScale{
		{"low (below the 2s deadline floor)", 1 * time.Second},
		{"default", 5 * time.Second},
		{"large", 30 * time.Second},
	}

	// Bounded at reclaimSlack's own documented value, same reasoning and
	// same values as the sibling file's reconnectLatencies -- see its doc
	// comment, not restated here.
	reconnectLatencies := []time.Duration{0, 250 * time.Millisecond, 900 * time.Millisecond}

	// stallSteps and roundTripSteps: same resolution as the sibling file.
	// D now ranges over [heartbeat, 2*heartbeat] -- the band that file
	// left open -- rather than [0, heartbeat).
	const stallSteps = 20
	const roundTripSteps = 5

	// simulateRecovery walks heartbeatLoop's own re-arm rule attempt by
	// attempt and returns the wall-clock time, measured from the last
	// successful heartbeat write, at which the next successful write
	// lands. offset tracks elapsed time since the stall itself started,
	// which (worst case, same framing as the sibling file) is the same
	// instant the first post-interval attempt starts touching the network
	// -- so heartbeat (T1) is added back in only once, at the end.
	simulateRecovery := func(m mode, heartbeat, deadline, retryInterval, D, dL, reconnect time.Duration) (recovery time.Duration, attempts int) {
		const maxAttempts = 100000 // generous: D <= 2*heartbeat <= 60s here, and each iteration advances offset by at least retryInterval (>0); see the file doc's "GENUINELY NEW" note for why this terminates
		var offset time.Duration
		payReconnect := false
		for attempts = 1; attempts <= maxAttempts; attempts++ {
			if payReconnect {
				offset += reconnect
			}
			remaining := D - offset
			if remaining <= 0 {
				// The stall is already over by the time this attempt starts
				// touching the network: an ordinary call. Modeled at its
				// own worst-case latency (`deadline`), not the swept dL --
				// see the file doc comment on why T4 is deadline, not dL,
				// even once the stall has genuinely cleared.
				return heartbeat + offset + deadline, attempts
			}
			dur := m.callDuration(remaining, dL, deadline)
			offset += dur + retryInterval
			payReconnect = m.retryPaysReconnect
		}
		return heartbeat + offset, attempts // unreachable in any swept case; see the panic-free cap note
	}

	// unsafeMySQL/unsafeMSSQL: running counts of grid points this file
	// measures as unsafe today (cleat#3258) for the two modes with no
	// closed form (see exactSafeBoundary's doc above). Pinned against a
	// literal below, same discipline as the sibling file's own falsification
	// footer ("exactly 30 of the 1890 grid points") -- a fixed, non-growing
	// population (this grid), not the kind of census CLAUDE.md warns rots.
	// t.Run below is never parallel, so a plain closure int is safe.
	var unsafeMySQL, unsafeMSSQL int

	cases := 0
	for _, m := range modes {
		for _, hs := range hbScales {
			deadline := dbCallDeadlineFor(hs.hb)
			retryInterval := heartbeatRetryIntervalFor(hs.hb)
			reclaimTimeout := minimumReclaimAfter(hs.hb)

			for _, reconnect := range reconnectLatencies {
				if !m.retryPaysReconnect && reconnect > 0 {
					continue // no-op dimension for this mode, same as the sibling file
				}
				for i := 0; i <= stallSteps; i++ {
					// [heartbeat, 2*heartbeat], endpoints included.
					D := hs.hb + time.Duration(i)*hs.hb/stallSteps
					for j := 0; j <= roundTripSteps; j++ {
						dL := time.Duration(j) * deadline / roundTripSteps // [0, deadline]
						cases++
						name := fmt.Sprintf("%s/hb=%s/D=%s/dL=%s/reconnect=%s",
							m.name, hs.name, D, dL, reconnect)
						t.Run(name, func(t *testing.T) {
							recovery, attempts := simulateRecovery(m, hs.hb, deadline, retryInterval, D, dL, reconnect)
							if attempts > 100000 {
								t.Fatalf("%s: simulateRecovery did not terminate within the attempt cap -- "+
									"the model's termination argument in this file's doc comment no longer holds for this grid point", name)
							}
							safe := recovery <= reclaimTimeout

							if m.exactSafeBoundary != nil {
								// postgres: a CHECKED claim, per point -- see
								// exactSafeBoundary's own doc for the algebra.
								boundary := m.exactSafeBoundary(deadline)
								expectSafe := D <= boundary
								if safe != expectSafe {
									t.Fatalf("%s: recovery=%v reclaimTimeout=%v -> safe=%v, but this file's "+
										"closed-form boundary (D<=%v) predicted safe=%v -- minimumReclaimAfter's "+
										"formula or reclaimSlack has changed since this boundary was derived "+
										"(cleat#3258); re-derive exactSafeBoundary's algebra before trusting "+
										"either this test or the production formula",
										name, recovery, reclaimTimeout, safe, boundary, expectSafe)
								}
								return
							}

							if !safe {
								// mysql/mssql: no closed form (see the file doc
								// comment) -- a KNOWN GAP (cleat#3258), counted
								// rather than failed per point, and the running
								// total is asserted against a pinned value
								// after the full sweep below.
								t.Logf("%s: KNOWN GAP (cleat#3258) -- recovery=%v exceeds reclaimTimeout(%v)=%v",
									name, recovery, hs.hb, reclaimTimeout)
								switch m.name {
								case "mysql (respects ctx, cut off at deadline)":
									unsafeMySQL++
								case "mssql (cancel-drain bound, then a reconnect on every retry)":
									unsafeMSSQL++
								}
								return
							}
						})
					}
				}
			}
		}
	}
	t.Logf("swept %d grid points over D in [heartbeat, 2*heartbeat] (3 modes x %d heartbeat scales x reconnect x %d stall steps x %d round-trip steps)",
		cases, len(hbScales), stallSteps+1, roundTripSteps+1)

	// Pinned counts of known-unsafe grid points (cleat#3258), measured
	// 2026-10-09. NOT a census of a growing population -- this grid is
	// fixed by the constants above, so these numbers do not drift with
	// unrelated repo growth the way CLAUDE.md warns a table/skip/export
	// count does. They DO move if minimumReclaimAfter, reclaimSlack, or
	// this file's own grid constants change -- that is the point: either
	// mismatch means cleat#3258 (or this file) needs a fresh look, not a
	// quiet update of the literal below. Re-derive by temporarily changing
	// the t.Logf lines above to t.Fatalf and reading `go test -v`'s own
	// FAIL count, or by instrumenting unsafeMySQL/unsafeMSSQL directly.
	const wantUnsafeMySQL = 186
	const wantUnsafeMSSQL = 685
	if unsafeMySQL != wantUnsafeMySQL {
		t.Fatalf("mysql known-gap count changed: got %d unsafe grid points, pinned at %d (cleat#3258) -- "+
			"minimumReclaimAfter, reclaimSlack, or this file's own grid has changed; re-derive before updating "+
			"the pinned literal", unsafeMySQL, wantUnsafeMySQL)
	}
	if unsafeMSSQL != wantUnsafeMSSQL {
		t.Fatalf("mssql known-gap count changed: got %d unsafe grid points, pinned at %d (cleat#3258) -- "+
			"minimumReclaimAfter, reclaimSlack, or this file's own grid has changed; re-derive before updating "+
			"the pinned literal", unsafeMSSQL, wantUnsafeMSSQL)
	}
}

// Falsification (applied by hand, verified, and reverted -- never
// committed, same discipline the sibling file records for itself): with
// pinnedReclaimSlack left at its real 1s value but minimumReclaimAfter's
// OWN reclaimSlack changed to 2*time.Second in setup.go (simulating a
// cleat#3258 fix that widens it), the postgres sub-tests go red --
// exactSafeBoundary's pinned boundary no longer matches the now-safer
// live formula, exactly the signal this file is built to give. Reverted
// before committing. Re-run this file's own command to reproduce:
//
//	go test ./cmd/cleat-worker/ -run TestReaperRecoveryGateHoldsAcrossTheParameterSpaceMultiAttempt -v
//
// The mysql/mssql pinned counts (wantUnsafeMySQL, wantUnsafeMSSQL above)
// were falsified the same way, separately: the same setup.go edit drops
// both counts (fewer grid points stay unsafe against a wider reclaimTimeout),
// so the hardcoded literals mismatch and fail loudly rather than silently
// passing a stale number.
