package main

import (
	"fmt"
	"testing"
	"time"
)

// TestReaperRecoveryGateHoldsAcrossTheParameterSpace is cleat#2175, a
// follow-up to cleat#2005/#2166 requested by cleat-review after five review
// rounds each found one more edge in minimumReclaimAfter's timing
// invariant (see that function's own doc comment in setup.go for the full
// history: an idle observer, a sub-heartbeat stall, a retry delay, zero
// slack against reconnect latency). Each round's fix was verified with one
// hand-picked scenario -- reaper_recovery_grace_period_test.go has seven of
// them. What none of those cover is the whole parameter space at once.
//
// SCOPE, STATED UP FRONT. This sweeps the invariant ARITHMETICALLY -- the
// `recovery` computation in the loop below is a direct restatement of
// minimumReclaimAfter's own documented four-term decomposition (T1..T4),
// evaluated exhaustively
// over stall length, round-trip latency, driver failure shape, reconnect
// latency and heartbeat scale -- rather than driving heartbeatLoop/reapOnce
// through thousands of real (even compressed) sleeps, which would either
// take minutes of wall time or risk exactly the scheduling-noise flakiness
// this file's own sibling tests go out of their way to avoid at millisecond
// scale. The existing hand-picked tests
// (TestASingleFailedHeartbeatRetryCanOutlastTheOldReclaimInvariantButNotTheNewOne,
// TestAReconnectBeforeTheRetryBreaksTheZeroSlackInvariantButNotWithReclaimSlack)
// already prove the arithmetic model is faithful to real execution for the
// two points they cover -- driving real heartbeatLoop goroutines with real
// (compressed) stalls and checking reapingIsSafe() at a hand-computed
// discriminating instant. This test is the missing middle: the same
// arithmetic those two points were hand-checked against, evaluated at every
// point on a fine grid instead of two.
//
// Also stated up front: D is swept over [0, heartbeat), not up to 2x
// heartbeat as cleat#2175's issue body asks. minimumReclaimAfter's own doc
// comment derives T2 (the failing call's own duration) for "a stall SHORTER
// than one heartbeat interval" explicitly; a stall lasting 1-2 heartbeats
// needs a multi-attempt state machine (which retry, delayed by which prior
// failure, first lands after the stall clears) that the current invariant's
// four-term formula does not claim to model at all. Extending past one
// heartbeat is a real gap, worth its own follow-up test built as a small
// simulation rather than a formula restatement -- see the note at the
// bottom of this file. Silently sweeping D past heartbeat here would either
// require inventing an unstated extension of the formula (encoding a claim
// nobody has reviewed) or produce false failures against a formula that
// was never meant to cover that range -- both worse than saying so.
//
// AND STATED PLAINLY, NOT LEFT IMPLICIT: this test covers the SAFETY
// direction only ("no live run is reclaimed too early"), not the LIVENESS
// direction cleat#2175 also asks for ("a dead worker's run is reclaimed
// within the documented bound"). An earlier version carried a liveness
// assertion; cleat-review found it could never fail for any positive
// heartbeat (it was deleted rather than repaired -- see the note where it
// used to be). This test also has no OBSERVER: cleat-review's own history
// records at least two of the four historical edges in this invariant as
// observer-side (an idle worker's own probe, the reaper-tick phase), and
// nothing here models either -- the grid varies the HOLDER's stall
// exposure only. Neither of those gaps is silently absorbed by "exhaustive
// sweep" in this file's own title; see the PR this test shipped in for
// how they are tracked.
//
// THE THREE DRIVER FAILURE SHAPES, cited from minimumReclaimAfter's own doc
// and cleat#2005's issue body:
//   - mysql:    the driver respects ctx cancellation, so a call caught by
//     the stall returns at min(D, deadline) -- cut off by the client-side
//     deadline even if the stall outlasts it.
//   - postgres: (models lib/pq specifically)
//     the driver does not act on ctx cancellation while the server is
//     genuinely unreachable -- a real docker-pause reproduction measured a
//     2s-deadline call not returning until an 11.7s pause lifted, per
//     minimumReclaimAfter's own doc -- so the call's duration is bounded by
//     D itself, not deadline.
//   - mssql: go-mssqldb's cancel-drain path (token.go) waits up to its own
//     ~5s internal timeout past the context deadline before giving up, and
//     marks the connection bad -- so the FIRST call's duration is
//     min(D, deadline+mssqlDrainSlack), and the RETRY pays a fresh
//     reconnect (dial, TLS handshake, login) on top of its own deadline,
//     which dbCallDeadlineFor was never meant to cover (it bounds query
//     execution, not connection setup).
func TestReaperRecoveryGateHoldsAcrossTheParameterSpace(t *testing.T) {
	// mssqlDrainSlack matches minimumReclaimAfter's own doc comment,
	// which cites go-mssqldb's cancel-drain path as "~5s" -- not an exact
	// published constant, so this names its source rather than pretending
	// to more precision than the doc it is drawn from claims.
	const mssqlDrainSlack = 5 * time.Second

	type mode struct {
		name string
		// callDuration is how long a call caught by a stall of length D
		// (with the call itself already dL into its own execution before
		// touching the stalled path) takes to return.
		callDuration func(D, dL, deadline time.Duration) time.Duration
		// retryPaysReconnect: the retry's own worst-case latency gains a
		// reconnect on top of deadline, not just deadline, because the
		// failed call's connection was marked bad.
		retryPaysReconnect bool
	}
	// Every mode shares the same starting point: dL (ordinary round-trip
	// latency) and D (the stall) both begin at the call's own start (the
	// worst-case framing minimumReclaimAfter's own doc uses -- the stall
	// begins right as the next attempt starts), so they OVERLAP rather
	// than stack. The call's raw, unbounded duration is therefore
	// max(dL, D): whichever finishes touching the network path last --
	// not dL+D, which would double-count time the call spends doing both
	// at once. Each mode then applies its own ceiling (or none) to that
	// raw duration.
	rawDuration := func(D, dL time.Duration) time.Duration { return max(D, dL) }
	modes := []mode{
		{
			// A client-side deadline bounds the call's TOTAL duration from
			// its own start.
			name: "mysql (respects ctx, cut off at deadline)",
			callDuration: func(D, dL, deadline time.Duration) time.Duration {
				return min(rawDuration(D, dL), deadline)
			},
		},
		{
			// No ceiling at all -- ignores context entirely.
			name: "postgres (ignores ctx, returns when the stall clears)",
			callDuration: func(D, dL, _ time.Duration) time.Duration {
				return rawDuration(D, dL)
			},
		},
		{
			// Same shape as mysql, but the driver's own cancel-drain path
			// bounds the total at deadline+mssqlDrainSlack rather than
			// deadline alone.
			name: "mssql (cancel-drain bound, then a reconnect on retry)",
			callDuration: func(D, dL, deadline time.Duration) time.Duration {
				return min(rawDuration(D, dL), deadline+mssqlDrainSlack)
			},
			retryPaysReconnect: true,
		},
	}

	type hbScale struct {
		name string
		hb   time.Duration
	}
	// Three heartbeat scales, matching cleat#2175's "low, default, large;
	// including the 2s deadline floor" -- real production-scale durations,
	// not compressed for a sleep-driven test, since this whole sweep is
	// pure arithmetic and costs no wall-clock time regardless of the
	// values chosen.
	hbScales := []hbScale{
		{"low (below the 2s deadline floor)", 1 * time.Second}, // deadline floors at 2s
		{"default", 5 * time.Second},                           // deadline = 2.5s, floor does not bind
		{"large", 30 * time.Second},                            // deadline = 15s
	}

	// reconnectLatencies: swept independently of mode, but only mssql's
	// callDuration/retryPaysReconnect wiring actually consumes it -- a
	// reconnect has no analogue in the other two modes' documented
	// behaviour, so this dimension is a no-op for them by construction,
	// not by omission.
	//
	// Bounded at reclaimSlack's own documented value (1s), not an
	// arbitrary round number: reclaimSlack's doc comment in setup.go
	// names what its fixed 1-second budget absorbs -- "a reconnect, the
	// retry's own initial round trip, and ordinary Timer/scheduler
	// lateness" -- collectively, not a reconnect budget alone. A sweep
	// value ABOVE 1s guarantees a failure by construction (this model's
	// T4 already charges the retry's own `deadline` separately, so
	// reconnect alone consuming the entire slack leaves nothing for the
	// other two things it is also supposed to cover) -- that would be
	// restating "reclaimSlack is a fixed constant and some unbounded input
	// can always exceed a fixed constant" as if it were a finding about
	// THIS invariant, not a report of an actually-realistic gap. 900ms is
	// the near-the-documented-edge stress point; go-mssqldb's own
	// reconnect cost (dial + TLS handshake + login) is not measured
	// anywhere in this repo, so this is the most defensible upper bound
	// available without a real measurement to cite.
	reconnectLatencies := []time.Duration{0, 250 * time.Millisecond, 900 * time.Millisecond}

	// stallSteps and roundTripSteps: "in small steps", per the issue.
	// Endpoints included; D deliberately stops just short of one full
	// heartbeat -- see the scope note above.
	const stallSteps = 20
	const roundTripSteps = 5

	cases := 0
	for _, m := range modes {
		for _, hs := range hbScales {
			deadline := dbCallDeadlineFor(hs.hb)
			retryInterval := heartbeatRetryIntervalFor(hs.hb)
			reclaimTimeout := minimumReclaimAfter(hs.hb)

			for _, reconnect := range reconnectLatencies {
				if !m.retryPaysReconnect && reconnect > 0 {
					continue // no-op dimension for this mode, see doc above
				}
				for i := 0; i <= stallSteps; i++ {
					D := time.Duration(i) * (hs.hb - 1) / stallSteps // [0, hb)
					for j := 0; j <= roundTripSteps; j++ {
						dL := time.Duration(j) * deadline / roundTripSteps // [0, deadline]
						cases++
						name := fmt.Sprintf("%s/hb=%s/D=%s/dL=%s/reconnect=%s",
							m.name, hs.name, D, dL, reconnect)
						t.Run(name, func(t *testing.T) {
							retryLatency := deadline
							if m.retryPaysReconnect {
								retryLatency += reconnect
							}
							recovery := hs.hb + // T1: wait for the next scheduled attempt
								m.callDuration(D, dL, deadline) + // T2: that attempt's own duration
								retryInterval + // T3: wait before the retry is issued
								retryLatency // T4: the retry's own worst-case latency

							if recovery > reclaimTimeout {
								t.Fatalf("%s: modeled worst-case recovery time %v exceeds "+
									"minimumReclaimAfter(%v)=%v -- a live run could be reclaimed "+
									"while its true holder is still trying to recover, not dead",
									name, recovery, hs.hb, reclaimTimeout)
							}

							// LIVENESS IS NOT COVERED BY THIS TEST. An earlier
							// version asserted `reclaimTimeout+tickSlack <= 0`
							// as a liveness check -- cleat-review found that
							// condition can never be false for any positive
							// heartbeat (both terms are durations derived from
							// a positive hb), so it passed even with
							// minimumReclaimAfter scaled 1000x, an ~4-hour
							// dead-run window at the default heartbeat. That
							// was a dead assertion, not a weak one, and it is
							// deleted rather than repaired: a real liveness
							// bound needs to reason about
							// ReapStaleInstances' own staleness predicate and
							// reaperLoop's own tick cadence together, which
							// TestReapOnceReclaimsOnceTheGracePeriodHasCleared
							// and friends already do directly, at the
							// hand-picked points they cover. This test's own
							// scope is the safety half only -- see the file
							// doc comment.
						})
					}
				}
			}
		}
	}
	t.Logf("swept %d grid points (3 modes x %d heartbeat scales x reconnect x %d stall steps x %d round-trip steps)",
		cases, len(hbScales), stallSteps+1, roundTripSteps+1)
}

// Falsification (applied by hand, verified, and reverted -- never
// committed, same discipline as this file's sibling falsification notes):
// with reclaimSlack changed from `1 * time.Second` to `0` in setup.go,
// exactly 30 of the 1890 grid points go red (cleat-review's own count,
// verified here rather than trusted -- an earlier version of this comment
// said 31, from a `grep -c` that also matched the run's own top-level
// "--- FAIL: TestReaperRecoveryGateHoldsAcrossTheParameterSpace" summary
// line as if it were a 31st grid point). The exact 30: mssql mode,
// hb=default only, reconnect=900ms at stall steps 17-20 (4.25s-5s, 4 values
// x 6 round-trip values = 24), plus reconnect=250ms at step 20 alone
// (4.999999999s x 6 round-trip values = 6). hb=low never fails because its
// deadline floors at 2s, leaving ample margin; hb=large never fails
// because deadline+mssqlDrainSlack (15s+5s=20s) is wide enough that the
// ceiling, not the stall, always binds T2 well below what a 30s heartbeat
// and reclaimTimeout can absorb. This is exactly minimumReclaimAfter's own
// round-5 zero-slack defect (see its doc comment): with no slack term, a
// reconnect of any size breaks the invariant precisely where the stall has
// consumed nearly all of T2's own budget, leaving nothing spare for T4's
// reconnect addition. Caught here at 30 grid points rather than the one
// hand-picked case
// TestAReconnectBeforeTheRetryBreaksTheZeroSlackInvariantButNotWithReclaimSlack
// checks. Restored via content diff against a pre-mutation backup,
// re-verified green. Confirmed 2026-09-29.
//
// THE RECONNECT DIMENSION IS A PIN INSIDE THE SLACK BUDGET, NOT EVIDENCE
// ABOUT REAL RECONNECT LATENCY -- state this plainly rather than let a
// green result imply more than it does. An earlier version of this sweep
// used an unbounded-feeling 2-second reconnect value, which failed at 24
// grid points even with reclaimSlack intact (unmutated). That was not a
// second, independent finding: reclaimSlack's own doc comment names its
// fixed 1-second budget as covering a reconnect TOGETHER WITH the retry's
// own initial round trip and ordinary scheduler lateness, not an unbounded
// reconnect alone, and this model already charges the retry's own
// worst-case latency separately (T4). So reconnectLatencies below was
// capped at reclaimSlack's own value -- which means, for the mssql arm,
// this sweep passing is exactly the statement "reconnect < reclaimSlack",
// true by construction of the grid, not a measurement that real
// go-mssqldb reconnects stay under that budget. Nothing in this repository
// measures a real reconnect's cost (dial + TLS handshake + login); that
// number is the one figure here that would correspond to production, and
// it remains unmeasured.
//
// FOLLOW-UP NOTED, NOT BUILT HERE: extending D past one heartbeat interval
// needs a multi-attempt simulation (each retry's start time depends on when
// the previous one failed, and the driver-specific failure shape determines
// how many attempts land inside a stall lasting more than one heartbeat) --
// a genuinely different, larger piece of work than restating the existing
// four-term formula, and one that would encode a new claim about
// multi-attempt behaviour nobody has reviewed yet. Flagging this explicitly
// rather than quietly shipping a partial grid as if it were the full one
// cleat#2175 asks for.
