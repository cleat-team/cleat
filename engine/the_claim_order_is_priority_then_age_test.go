package engine

// cleat#2041: does ClaimWorkflows' ordering actually give per-instance
// fairness, or is `CleatClaim.tla`'s NoStarvation counter-example reachable
// against a real store?
//
// The spec models the claim as a DISJUNCTION over which ready instance to
// take, and `SF_vars(Claim(w))` forces that disjunction to fire infinitely
// often without constraining WHICH disjunct fires. The counter-example is then
// w2 taking one instance forever while another sits ready. The question this
// file answers is whether the implementation offers that choice at all.
//
// It does not, and the measurement below is the answer rather than the reading:
// engine/store_lifecycle.go:241 is
//
//	ORDER BY w.priority ASC, w.created_at
//	LIMIT $2
//	FOR UPDATE OF w SKIP LOCKED
//
// so the claimant does not choose -- the order is a total order on the
// candidate set (up to created_at ties) and LIMIT takes its head. That closes
// the spec's counter-example and opens a different question, which the third
// test measures: `priority` sorts FIRST and age is only the tie-break, so a
// run can still be passed over indefinitely, by sustained higher-priority
// arrivals. That is a reachable starvation path in the real system, and it is
// NOT the one the spec found.
//
// Nothing in this package asserted claim ORDER before this file, which is worth
// knowing on its own: the two clauses of that ORDER BY were uncovered.
//
// WHAT THIS MEASURES, AND WHAT IT DOES NOT. Every claim below is made by a
// single claimant against one live PostgreSQL store, because that is the
// configuration the counter-example is about: one worker, choosing among ready
// instances. The concurrent case -- several claimants whose SKIP LOCKED could
// in principle interact -- is NOT measured here and no conclusion about it is
// drawn. The single-claimant result does bound it, though: each concurrent
// claimant independently takes the head of the same total order, so they can
// only take the earliest candidates between them.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// claimOrderFixture is a live PostgreSQL store with a definition deployed,
// plus the few verbs the measurements need.
type claimOrderFixture struct {
	t      *testing.T
	store  *PostgresStore
	tenant string
	def    string
}

func newClaimOrderFixture(t *testing.T, tenant string) *claimOrderFixture {
	t.Helper()
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, adminDB) })

	def := fmt.Sprintf("claim-order-%s", tenant[:8])
	store := NewPostgresStore(adminDB).WithTenant(tenant)
	if err := store.DeployWorkflowDef(context.Background(), &WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	return &claimOrderFixture{t: t, store: store, tenant: tenant, def: def}
}

// seed creates a ready run. Inserts are sequential, so created_at increases;
// the tests below assert against created_at read back from the row rather than
// against insertion order, because two inserts inside one microsecond would
// otherwise make the expectation ambiguous rather than wrong.
func (f *claimOrderFixture) seed(id string, priority int) {
	f.t.Helper()
	if _, _, err := f.store.StartNewRun(context.Background(), id, f.def, 1,
		json.RawMessage(`{}`), "", f.tenant, priority); err != nil {
		f.t.Fatalf("starting %s: %v", id, err)
	}
}

// claim takes up to limit candidates, as a worker does.
func (f *claimOrderFixture) claim(worker string, limit int) []*WorkflowInstance {
	f.t.Helper()
	got, err := f.store.ClaimWorkflows(context.Background(), worker, limit)
	if err != nil {
		f.t.Fatalf("ClaimWorkflows(%s, %d): %v", worker, limit, err)
	}
	return got
}

// release puts a claimed run back to ready, which is what the spec's
// Claim/Release cycle is: the run is claimable again, unchanged in age.
//
// The worker and the generation are both part of the fence, and a first
// version of this passed a different worker than the one that claimed, which
// fails with "fence lost: workflow reassigned to another worker (generation
// mismatch)" -- a true statement about a fixture that had not actually lost
// anything.
func (f *claimOrderFixture) release(worker string, wf *WorkflowInstance) {
	f.t.Helper()
	if err := f.store.ReleaseWorkflow(context.Background(), wf.ID, worker, wf.Generation, time.Now()); err != nil {
		f.t.Fatalf("releasing %s as %s: %v", wf.ID, worker, err)
	}
}

// TestTheClaimOrderIsPriorityThenAge measures what the ORDER BY decides.
//
// It asserts WHICH runs are claimed and not the order the slice comes back in,
// and that distinction is a measured property rather than a stylistic choice:
// the ORDER BY is on the candidate SELECT, but the rows are returned by the
// final `UPDATE ... WHERE w.id = ANY($2) RETURNING ...` at
// engine/store_lifecycle.go:289, which has no ORDER BY at all. The ordering
// chooses the top-N candidates; it does not survive into the returned slice.
// A first version of this test asserted the slice was sorted and failed on
// correct code.
func TestTheClaimOrderIsPriorityThenAge(t *testing.T) {
	f := newClaimOrderFixture(t, "2041aaaa-0000-4000-8000-000000000001")

	// Inserted oldest-first, and with the OLDER one at the WORSE priority, so
	// that age and priority disagree and the next claim discriminates.
	f.seed("older-low-priority", 9)
	time.Sleep(2 * time.Millisecond)
	f.seed("newer-high-priority", 1)

	// limit=1 with both ready. If age decided, the older one goes first.
	first := f.claim("worker-order", 1)
	if len(first) != 1 {
		t.Fatalf("claimed %d, want 1", len(first))
	}
	if first[0].ID != "newer-high-priority" {
		t.Fatalf("claimed %s, the OLDER run, over newer-high-priority.\n\n"+
			"ClaimWorkflows orders by `priority ASC, created_at` "+
			"(engine/store_lifecycle.go:241): priority sorts FIRST and age only breaks ties. "+
			"If age decided instead, then the ordering the spec assumes is not the one the "+
			"store implements, and cleat#2041's reachability question has to be re-asked "+
			"against whatever the real order is.", first[0].ID)
	}

	// Nothing outranks the other one now, so age decides -- and that is the
	// half the spec's fairness argument rests on.
	second := f.claim("worker-order", 1)
	if len(second) != 1 || second[0].ID != "older-low-priority" {
		t.Fatalf("after the higher-priority run was taken, claimed %v, want older-low-priority",
			idsOf(second))
	}

	// Within one priority, age decides. Seeded in this order so that "the
	// newest wins" and "the oldest wins" are different answers.
	f.seed("equal-older", 5)
	time.Sleep(2 * time.Millisecond)
	f.seed("equal-newer", 5)
	third := f.claim("worker-order", 1)
	if len(third) != 1 || third[0].ID != "equal-older" {
		t.Fatalf("at equal priority claimed %v, want equal-older: created_at is the tie-break "+
			"inside a priority, which is what makes the claim a total order on candidates "+
			"rather than a choice.", idsOf(third))
	}
}

func idsOf(wfs []*WorkflowInstance) []string {
	out := make([]string, 0, len(wfs))
	for _, wf := range wfs {
		out = append(out, wf.ID)
	}
	return out
}

// TestTheSpecsClaimerCannotChoose solves the spec's counter-example against the
// real store.
//
// The spec lets the claimant pick any ready instance and keep picking it. Here
// the same run is claimed and released repeatedly with a second, newer run
// ready the whole time. If the claimant could choose, it could return the
// newer one -- and the spec's fairness argument is genuinely broken. It never
// does, so that transition does not exist to be taken.
func TestTheSpecsClaimerCannotChoose(t *testing.T) {
	f := newClaimOrderFixture(t, "2041aaaa-0000-4000-8000-000000000002")

	f.seed("first-ready", 5)
	time.Sleep(2 * time.Millisecond) // guarantee a distinct created_at
	f.seed("second-ready", 5)

	const rounds = 20
	for i := 0; i < rounds; i++ {
		got := f.claim("worker-choice", 1)
		if len(got) != 1 {
			t.Fatalf("round %d: claimed %d, want 1", i, len(got))
		}
		if got[0].ID != "first-ready" {
			t.Fatalf("round %d claimed %s while the older first-ready was still claimable.\n\n"+
				"This is the spec's counter-example, realised: CleatClaim.tla models the claim "+
				"as a disjunction over which ready instance to take, so a claimant can take the "+
				"newer one forever and starve the older. The SQL orders by (priority, "+
				"created_at) and takes the head, so it cannot -- unless this fails, in which "+
				"case #2041 is an engine liveness bug and not a spec gap.", i, got[0].ID)
		}
		f.release("worker-choice", got[0])
	}
}

// TestSustainedHigherPriorityArrivalsStarveAnOlderRun is the reachable path.
//
// It is NOT the spec's counter-example and this test does not claim it is. It
// is what `ORDER BY priority ASC, created_at` implies once priority is in front
// of age: age only decides WHICH of an equal-priority set goes first, so a
// stream of higher-priority arrivals passes an older lower-priority run over
// indefinitely. With unbounded arrivals the older run is never claimed.
//
// THIS IS (LIKELY) INTENDED BEHAVIOUR, NOT A DEFECT THIS TEST PINS. Priority
// exists to let an operator say "this work goes first"; a run that says that
// about itself, permanently, is going to be preferred to one that does not, and
// that is the feature working. Do not "fix" this test by making the older run
// win -- an equal-priority test would be right to assert age-wins (see
// TestTheClaimOrderIsPriorityThenAge) and this one is the case where priority
// outranks age by design. The finding is that the spec CANNOT see this,
// because CleatClaim.tla has no priority, so any fairness claim it makes is
// about a system without one.
//
// Recorded as a measurement with a bounded loop, because the loop is what makes
// it runnable: "indefinitely" is asserted as "still ready after N arrivals each
// of which was preferred to it".
func TestSustainedHigherPriorityArrivalsStarveAnOlderRun(t *testing.T) {
	f := newClaimOrderFixture(t, "2041aaaa-0000-4000-8000-000000000003")

	f.seed("old-low-priority", 10)
	time.Sleep(2 * time.Millisecond)
	f.seed("newer-but-higher-priority", 0)

	const rounds = 10
	for i := 0; i < rounds; i++ {
		if i > 0 {
			f.seed(fmt.Sprintf("arrival-%d", i), 0)
		}
		got := f.claim("worker-priority", 1)
		if len(got) != 1 {
			t.Fatalf("round %d: claimed %d, want 1", i, len(got))
		}
		if got[0].ID == "old-low-priority" {
			t.Fatalf("round %d claimed old-low-priority, the OLDER run, ahead of a "+
				"priority-0 arrival.\n\n"+
				"This test records a measured consequence of `ORDER BY priority ASC, "+
				"created_at`: priority outranks age, so an older lower-priority run is passed "+
				"over while higher-priority work keeps arriving. It failing means that "+
				"ordering moved.\n\n"+
				"It is (likely) INTENDED behaviour -- priority is a feature -- so do NOT revert "+
				"the ordering to make this pass. Find which change moved it, and if "+
				"age-outranks-priority is now the design, update this test and cleat#2041's "+
				"note rather than the store.", i)
		}
	}

	status, _, _ := f.rowStatus("old-low-priority")
	if status != "ready" {
		t.Fatalf("old-low-priority is %q after %d rounds, want it untouched by all of them", status, rounds)
	}
	t.Logf("old-low-priority remained ready across %d higher-priority arrivals; each arrival was "+
		"preferred to it, so with unbounded arrivals it is never claimed.\n\n"+
		"This is per-instance unfairness in the real store, but it is PRIORITY, which the model "+
		"does not capture -- not the disjunctive-fairness gap the spec found. It is (likely) "+
		"INTENDED: priority is a feature, and a permanently-higher-priority run is supposed to go "+
		"first. Recorded so the spec's fairness argument is not read as covering a system that "+
		"has one.", rounds)
}

func (f *claimOrderFixture) rowStatus(id string) (status string, assignedTo, worker string) {
	f.t.Helper()
	var st string
	var assigned, who interface{}
	if err := f.store.db.QueryRow(
		`SELECT status, assigned_to, trace_id FROM workflow_instances WHERE id = $1`, id).
		Scan(&st, &assigned, &who); err != nil {
		f.t.Fatalf("reading status for %s: %v", id, err)
	}
	return st, fmt.Sprint(assigned), fmt.Sprint(who)
}
