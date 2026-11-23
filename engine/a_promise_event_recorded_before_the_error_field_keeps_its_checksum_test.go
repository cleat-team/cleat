package engine

import "testing"

// A history recorded before cleat#4335 has no Err on its promise events, and its checksum chain must still verify: the payload gains an "error"
// key ONLY when Err is set, so an Err-free create, resolved or rejected hashes exactly as it did.
//
// The literals below were computed on b2ed57e7, the commit before this change (a scratch test in a worktree at that commit, the same records),
// not by the code under test. A test that compared computeEventChecksum with itself would pass when the payload ALWAYS carried an "error" key,
// even an empty one, which changes the checksum of every promise event ever recorded and breaks verification of every old history: the
// whole engine package stayed green with exactly that mutation (cleat-review, #4401, mutant R7).
func TestAPromiseEventRecordedBeforeTheErrorFieldKeepsItsChecksum(t *testing.T) {
	const prev = "00112233445566ff"
	cases := []struct {
		name           string
		rec            EventRecord
		first, chained string // computed on b2ed57e7
	}{
		{"create", EventRecord{Step: 3, EventType: EventTypeCreatePromise, PromiseName: "approval", PromiseID: "p-1", TimestampMs: 1000},
			"c74a306d218216cb", "b2c4c2abcc073904"},
		{"resolved", EventRecord{Step: 4, EventType: EventTypePromiseResolved, Service: PromiseEventService, Op: PromiseOpResolve,
			PromiseID: "p-1", PromiseResult: `"done"`, TimestampMs: 1001},
			"d3abbe4c777393de", "28136bbfc40625d3"},
		{"rejected", EventRecord{Step: 5, EventType: EventTypePromiseRejected, Service: PromiseEventService, Op: PromiseOpReject,
			PromiseID: "p-1", PromiseError: "declined", TimestampMs: 1002},
			"996d32f90690e407", "64976ec211683501"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeEventChecksum(c.rec, ""); got != c.first {
				t.Errorf("first-in-chain checksum = %s, want %s (as computed before Err existed): an Err-free %s now hashes differently, so every history recorded before this fails verification", got, c.first, c.name)
			}
			if got := computeEventChecksum(c.rec, prev); got != c.chained {
				t.Errorf("chained checksum = %s, want %s", got, c.chained)
			}

			// The other direction: a recorded failure is inside the checksum, so editing or dropping it breaks the chain.
			failed := c.rec
			failed.Err = "store: connection refused"
			withErr := computeEventChecksum(failed, "")
			if withErr == c.first {
				t.Errorf("a %s carrying Err hashes the same as one without (%s): an edited or removed failure would not break the chain", c.name, withErr)
			}
			failed.Err = "store: a different failure"
			if other := computeEventChecksum(failed, ""); other == withErr {
				t.Errorf("two different Err values hash the same (%s)", other)
			}
		})
	}
}
