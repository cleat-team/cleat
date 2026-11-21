//go:build chaos

package chaos

// Ops whose effect lands where the recording service cannot see it (cleat#4241): signal_workflow, and the three cron calls.
//
// The harness's oracle counts what the recording service received. These calls make no request to it: a signal lands in the signal table and a
// schedule in workflow_schedules. So each op's effect is READ BACK from its table after the run (signalRows, scheduleRows), and the scenario says
// what the table must hold (outsideExpect). A fault-free run asserts it unconditionally, so an op that did not do what it claims reads as a failed
// precondition and not as a green; a run that ended done after a fault asserts it too.
//
// A row count alone cannot see every double effect, and that is why two of the three cron scenarios change the world between the run and its
// replay. schedule_cron derives its id from (tenant, workflow, step) and creates the row idempotently, so a replay that wrongly ran it FRESH
// finds the row it made and leaves the count at one. What a fresh replay does do is write the row again where the world no longer has it, and
// delete again a row that was put back. So:
//
//	crons      the harness deletes cron C's row after the run created it, and puts cron D's row back after the run deleted it. The run must
//	           end with C absent and D present: a replay that re-created C or re-deleted D shows in the table, which a count of "rows the run
//	           made" never would.
//	cron_list  the harness adds a schedule to the table AFTER the run listed it. A replay must answer the list it recorded, not the table as it
//	           is now: the count the guest held is reported to the service, and the existing oracle fails a run in which one op reported two
//	           different held values.
//
// Every one of these helpers waits on what is DURABLE (a row, an event in event_history), never on what the worker has merely done: a worker
// killed before its event was flushed owes the replay nothing, and acting before that would make the replay's fresh read look like a bug.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// cronWorkflowOf is the workflow name every cron op of a run schedules, so its rows are told apart from the rest of the table.
func cronWorkflowOf(run string) string { return "chaos-cron-" + run }

// signalRows counts the signals the run received, by name. Only signals the script sent itself ("sig-<name>") are the oracle's business.
func signalRows(db *sql.DB, run string) map[string]int {
	m := map[string]int{}
	rows, err := db.Query(`SELECT signal_name, count(*) FROM workflow_signals WHERE workflow_id = $1 GROUP BY signal_name`, run)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		if rows.Scan(&name, &n) == nil {
			m[name] = n
		}
	}
	return m
}

// scheduleRows counts the schedule rows of the run's cron workflow, by the op name the script put in the input ({"n":"C"}).
func scheduleRows(db *sql.DB, run string) map[string]int {
	m := map[string]int{}
	rows, err := db.Query(`SELECT coalesce(input->>'n', ''), count(*) FROM workflow_schedules WHERE def_name = $1 GROUP BY 1`, cronWorkflowOf(run))
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		if rows.Scan(&name, &n) == nil {
			m[name] = n
		}
	}
	return m
}

// forgetSchedules removes the rows a run left, so a long-lived harness database does not accumulate them.
func forgetSchedules(db *sql.DB, run string) {
	_, _ = db.Exec(`DELETE FROM workflow_schedules WHERE def_name = $1`, cronWorkflowOf(run))
}

// outsideExpect is what each scenario that has outside effects must leave behind, by key: "signal:<name>" is the rows under the signal "sig-<name>",
// "cron:<n>" the schedule rows whose input names n. A key with no row counts zero, so 0 is a real expectation ("this one must be gone").
var outsideExpect = map[string]map[string]int{
	"signals": {"signal:S": 1, "signal:T": 1},
	// C was deleted by the harness after the run created it, D was put back after the run deleted it, E was created and never touched.
	"crons": {"cron:C": 0, "cron:D": 1, "cron:E": 1},
	// C is the run's own; "late" is the harness's, added after the run listed, and must survive.
	"cron_list": {"cron:C": 1, "cron:late": 1},
}

// heldExpect is the value each request of a read op must carry, by scenario and operation. A replayed list is observable only through the request
// made after it, and "one op reported two different held values" (verifyReclaim) cannot see a replay that is WRONG every time it is asked: in a
// "before" case the aborted first arrival is not in the request list, so the only value the service sees is the wrong one. The count is
// deterministic here (the run's own schedule, and the late one is added only after the list is durable), so it is compared with the right answer.
var heldExpect = map[string]map[string]string{
	"cron_list": {"L": "n=1", "L2": "n=1"},
}

// outsideViolations compares the tables a FINISHED run left with the scenario's expectation. UNMEASURED is the caller's business: a run that did not
// finish has performed an unknown prefix of the script.
func outsideViolations(sc scenario, o outcome) []string {
	var v []string
	for _, r := range o.Requests {
		if want, ok := heldExpect[sc.Name][r.Op]; ok && r.Held != want {
			v = append(v, fmt.Sprintf("request %q carried held value %q, want %q: a replayed read answered something other than the record", r.Op, r.Held, want))
		}
	}
	want, ok := outsideExpect[sc.Name]
	if !ok {
		return v
	}
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kind, name, found := strings.Cut(k, ":")
		if !found {
			v = append(v, fmt.Sprintf("the scenario's expectation key %q is not kind:name", k))
			continue
		}
		var got int
		switch kind {
		case "signal":
			got = o.Signals["sig-"+name]
		case "cron":
			got = o.Schedules[name]
		default:
			v = append(v, fmt.Sprintf("the scenario's expectation key %q has an unknown kind %q", k, kind))
			continue
		}
		if got != want[k] {
			v = append(v, fmt.Sprintf("%s: %d row(s) after the run, want %d", k, got, want[k]))
		}
	}
	return v
}

// waitFor polls until cond holds, for at most runBudget, and says whether it did. It is for the helpers that act beside a run.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(runBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// cronRow is a schedule row as the harness needs it to put it back.
type cronRow struct {
	name, def, expr, input, tz, tenant string
}

func readCronRow(db *sql.DB, run, n string) (cronRow, bool) {
	var r cronRow
	err := db.QueryRow(`SELECT name, def_name, cron_expression, input::text, timezone, tenant_id::text FROM workflow_schedules
		WHERE def_name = $1 AND input->>'n' = $2`, cronWorkflowOf(run), n).Scan(&r.name, &r.def, &r.expr, &r.input, &r.tz, &r.tenant)
	return r, err == nil
}

func cronRowCount(db *sql.DB, run, n string) int {
	var c int
	_ = db.QueryRow(`SELECT count(*) FROM workflow_schedules WHERE def_name = $1 AND input->>'n' = $2`, cronWorkflowOf(run), n).Scan(&c)
	return c
}

// editCronsBetweenRunAndReplay is the Drive of the "crons" scenario: it changes the schedule table AFTER the run's own ops, so a replay that acts
// on the table instead of answering from its record leaves a mark. See the header. It must only read and write through the database.
func editCronsBetweenRunAndReplay(db *sql.DB, run string) string {
	// C: once the run has created it, take it away. A replay that scheduled it again would bring it back.
	if !waitFor(func() bool { return cronRowCount(db, run, "C") == 1 }) {
		return "cron C's row never appeared, so it was not taken away"
	}
	_, _ = db.Exec(`DELETE FROM workflow_schedules WHERE def_name = $1 AND input->>'n' = 'C'`, cronWorkflowOf(run))
	// D: remember the row the run created, wait for the run to delete it, then put it back. A replay that deleted it again would take it away.
	var d cronRow
	if !waitFor(func() bool { var ok bool; d, ok = readCronRow(db, run, "D"); return ok }) {
		return "cron D's row never appeared, so there was nothing to put back"
	}
	if !waitFor(func() bool { return cronRowCount(db, run, "D") == 0 }) {
		return "cron D's row was never deleted by the run, so it was not put back"
	}
	if _, err := db.Exec(`INSERT INTO workflow_schedules (name, def_name, cron_expression, input, timezone, tenant_id, next_run_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6::uuid, '2099-01-01T00:00:00Z')`, d.name, d.def, d.expr, d.input, d.tz, d.tenant); err != nil {
		return "putting cron D's row back failed: " + err.Error()
	}
	return ""
}

// addACronAfterTheRunListed is the Drive of the "cron_list" scenario: once the run's list_crons event is DURABLE, add a schedule the list cannot
// have contained. Any later answer to the same list that includes it was read from the table and not from the record.
func addACronAfterTheRunListed(db *sql.DB, run string) string {
	if !waitFor(func() bool {
		var n int
		_ = db.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1 AND event_type = 'list_crons'`, run).Scan(&n)
		return n > 0
	}) {
		return "the run's list_crons event never became durable, so no schedule was added after it"
	}
	if _, err := db.Exec(`INSERT INTO workflow_schedules (name, def_name, cron_expression, input, timezone, tenant_id, next_run_at)
		VALUES ($1, $2, '0 0 1 1 *', '{"n":"late"}'::jsonb, 'UTC', $3::uuid, '2099-01-01T00:00:00Z')`, "late-"+run, cronWorkflowOf(run), defaultTenant); err != nil {
		return "adding the late schedule failed: " + err.Error()
	}
	return ""
}

// ---- durable_send (cleat#4241) ----
//
// A send is the one effect here the recording service CAN see (it dispatches to the service, as op "send_<name>"), but it is not exactly-once like
// every other effect the generic oracle counts. The engine records the send FIRST and dispatches it on a goroutine after, and a replay answers
// success and dispatches nothing. So a kill between the record and the dispatch loses the send for good, and nothing may ever make it twice: AT MOST
// once. The generic sweeps demand exactly once and would call that loss a failure, so the send scenarios have their own list and the two
// verifiers read the exemption off the scenario's own `send` ops (atMostOnceOps), which leaves every scenario without one exactly as it was.

// fireAndForgetScenarios are swept by TestChaosAFireAndForgetSendIsNeverRepeated... and by nothing else.
var fireAndForgetScenarios = []scenario{
	// A send between two calls, and a second one after: a kill before a send finds nothing to repeat, a kill after one forces a replay of it.
	{"sends", []op{{Op: "call", Name: "A"}, {Op: "send", Name: "S"}, {Op: "call", Name: "B"}, {Op: "send", Name: "T"}, {Op: "call", Name: "C"}}, nil},
}

// atMostOnceOps names the service operations of the scenario's fire-and-forget sends.
func atMostOnceOps(sc scenario) map[string]bool {
	m := map[string]bool{}
	for _, o := range sc.Ops {
		if o.Op == "send" {
			m["send_"+o.Name] = true
		}
	}
	return m
}

// callsAfterSend names the requests of the plain synchronous calls the script makes AFTER the fire-and-forget send whose request is sendOp
// ("send_S"). The send is dispatched on a goroutine, so the body is already running those calls while the send is held at a boundary: one of them
// may have been performed, and not yet recorded, when the kill landed (the body moves past a call only once its record is durable, so at most ONE
// call is in that state). Empty when sendOp is not a send.
//
// SCOPE, and it is deliberately narrow: only op "call". A retried call (call_retry) can show more than one success inside that single window, so
// the allowance verifyReclaim builds on this ("one extra success, in total") does not describe it, and an op this does not know is outside the
// allowance, so a defect there is still reported. No send scenario has a retried call after a send today. If one is added, extend this on purpose,
// counting per call, rather than relying on it to false-red (cleat-review3, #4280).
func callsAfterSend(sc scenario, sendOp string) map[string]bool {
	m := map[string]bool{}
	after := false
	for _, o := range sc.Ops {
		if o.Op == "send" && "send_"+o.Name == sendOp {
			after = true
			continue
		}
		if after && o.Op == "call" {
			m[o.Name] = true
		}
	}
	return m
}

// withoutOps drops the named operations from an order, so the order of the effects that DO happen exactly once is compared without a send that a
// kill may legitimately have lost.
func withoutOps(order []string, drop map[string]bool) []string {
	var out []string
	for _, op := range order {
		if !drop[op] {
			out = append(out, op)
		}
	}
	return out
}

// tallyOnce counts the successful requests of one operation.
func tallyOnce(reqs []request, op string) int {
	n := 0
	for _, r := range reqs {
		if r.Op == op && !r.Failed {
			n++
		}
	}
	return n
}

// TestChaosAFireAndForgetSendIsNeverRepeatedByAReclaim kills the worker at every boundary of the send scenario and reclaims: each send happens at most
// once, every call exactly once, and the run ends as the fault-free one did.
func TestChaosAFireAndForgetSendIsNeverRepeatedByAReclaim(t *testing.T) {
	runBoundariesOver(t, fireAndForgetScenarios, "reclaim", verifyReclaim)
}

// TestChaosAFireAndForgetSendIsNeverMadeByATerminatedRun terminates the run after the kill: the defer segment dispatches nothing the body had not.
func TestChaosAFireAndForgetSendIsNeverMadeByATerminatedRun(t *testing.T) {
	runBoundariesOver(t, fireAndForgetScenarios, "terminate", verifyTerminate)
}
