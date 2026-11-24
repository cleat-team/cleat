//go:build chaos

package chaos

// A worker that exits before the boundary, and what PostgreSQL's connection slots held (cleat#4291).
//
// The 2026-11-21 nightly seeded step lost four consecutive cases of one generated script to a worker that REFUSED TO START for want of a connection
// slot (`pq: remaining connection slots are reserved for roles with the SUPERUSER attribute (53300)`). The harness did not notice: it waited runBudget for
// a boundary no run could reach, killed the already-dead worker, waited runBudget again for a terminal status, and reported "the fault never fired (the
// run did not reach the boundary)". That is honest and it reads like a generator problem, and it cost 240 s a case, 16 minutes for four.
//
// Now: execute notices the first worker's exit, ends the case at once, and runFault reports UNMEASURED naming the exit status and the worker's last
// ERROR line, with pg_stat_activity as it was at that moment, so a recurrence shows what holds the slots. connSnapshot is also logged before each case
// and at the boundary.
//
// Each piece has its own known positive below. A connection count that reads 0 because it looked at the wrong database is the way this kind of probe
// fails silently, so one test asserts the snapshot SEES a live worker, and another asserts a failed query is UNMEASURED and not a zero.

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChaosExitReasonNamesTheStatusAndTheLastErrorLine(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	mk := func(log string, err error) *worker {
		return &worker{log: &strings.Builder{}, exited: closed, exitErr: err}
	}

	w := mk("", errors.New("exit status 1"))
	w.log.WriteString(`{"level":"INFO","msg":"starting worker"}` + "\n")
	w.log.WriteString(`{"time":"t","level":"ERROR","msg":"refusing to start: could not register","error":"pq: remaining connection slots are reserved for roles with the SUPERUSER attribute (53300)"}` + "\n")
	w.log.WriteString(`{"level":"INFO","msg":"after the error"}` + "\n")
	got := w.exitReason()
	for _, want := range []string{"exit status 1", "last ERROR:", "refusing to start: could not register", "53300"} {
		if !strings.Contains(got, want) {
			t.Errorf("exitReason() = %q, want it to contain %q", got, want)
		}
	}
	// CONTROL: it reads the LAST error line, not the first and not a later INFO line.
	w2 := mk("", errors.New("exit status 1"))
	w2.log.WriteString(`{"level":"ERROR","msg":"the first error"}` + "\n" + `{"level":"ERROR","msg":"the last error"}` + "\n" + `{"level":"INFO","msg":"later info"}` + "\n")
	if r := w2.exitReason(); !strings.Contains(r, "the last error") || strings.Contains(r, "the first error") || strings.Contains(r, "later info") {
		t.Errorf("exitReason() did not pick the last ERROR line: %q", r)
	}
	// A worker that logged no ERROR line says so instead of inventing one, and a clean exit is not an error.
	if r := mk("", nil).exitReason(); !strings.Contains(r, "exit status 0") || !strings.Contains(r, "logged no ERROR line") {
		t.Errorf("exitReason() for a silent clean exit = %q", r)
	}
}

func TestChaosAConnectionSnapshotThatFailsIsUnmeasuredNotZero(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: sql.Open: %v", err)
	}
	_ = db.Close() // a closed handle: every query on it fails
	total, line, qerr := connSnapshot(db)
	if qerr == nil {
		t.Fatalf("a snapshot through a closed handle reported no error: total=%d line=%q", total, line)
	}
	if !strings.HasPrefix(line, "UNMEASURED") {
		t.Errorf("a failed snapshot's line = %q, want it to start with UNMEASURED (a failed query is not a count of zero)", line)
	}
}

// KNOWN POSITIVE for the snapshot: it must see a live worker's connection pools. A snapshot that read 0 here (the wrong database, or a filter that
// drops the rows) would pass every other test and measure nothing.
func TestChaosAConnectionSnapshotSeesALiveWorker(t *testing.T) {
	e := newEnv(t)
	before, _, err := connSnapshot(e.db)
	if err != nil {
		t.Fatalf("UNMEASURED: the baseline snapshot failed: %v", err)
	}
	w := startWorker(t, e.bin, "chaos-q-conn-"+time.Now().Format("150405.000000"), e.svc.srv.URL)
	var during int
	var line string
	if !waitFor(func() bool {
		select {
		case <-w.exited:
			return true // the worker is gone: stop waiting, the assertion below reports it
		default:
		}
		during, line, _ = connSnapshot(e.db)
		return during >= before+5
	}) {
		t.Fatalf("UNMEASURED: the snapshot never rose by 5 connections above its baseline of %d (last: %d), so it either looks at the wrong database or the worker never connected: %s", before, during, line)
	}
	select {
	case <-w.exited:
		t.Fatalf("UNMEASURED: the worker exited before it could be seen: %s", w.exitReason())
	default:
	}
	if !strings.Contains(line, "max_connections") || !strings.Contains(line, "biggest groups") {
		t.Errorf("the snapshot line lacks its parts: %q", line)
	}
	t.Logf("baseline %d, with a live worker %d: %s", before, during, line)
	w.kill()
	after, _, _ := connSnapshot(e.db)
	t.Logf("after the kill: %d (informational: killed workers' connections should not linger)", after)
}

// KNOWN POSITIVE for the fail-fast path: a worker that cannot reach its database must end the case in seconds, not 240, and say why.
func TestChaosAWorkerThatCannotStartEndsTheCaseAtOnceAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	var sc scenario
	for _, s := range scenarios {
		if s.Name == "calls" {
			sc = s
		}
	}
	if sc.Name == "" {
		t.Fatal("PRECONDITION FAILED: no scenario named calls")
	}
	// The flags are appended after the harness's own, and the later --db wins. Port 1 is closed, so the worker cannot register and refuses to start.
	f := &fault{Op: "A", Nth: 1, Mode: "before", Then: "reclaim", Flags: []string{"--db", "postgres://cleat:wrong@127.0.0.1:1/cleat?sslmode=disable&connect_timeout=2"}}
	start := time.Now()
	o := execute(t, e, sc, f)
	took := time.Since(start)

	if took > 60*time.Second {
		t.Errorf("the case took %s: it waited for a worker that had already exited (runBudget is %s, twice over before this change)", took.Round(time.Second), runBudget)
	}
	if o.Applied {
		t.Fatalf("PRECONDITION FAILED: the fault fired, so the worker did start and this case measures nothing about an early exit: status=%s", o.Status)
	}
	if o.Status != "WORKER-EXITED" {
		t.Errorf("status = %q, want WORKER-EXITED (the case should have ended on the worker's exit, not on a timeout)", o.Status)
	}
	if !strings.Contains(o.WorkerExit, "last ERROR:") {
		t.Errorf("WorkerExit = %q, want it to name the worker's last ERROR line", o.WorkerExit)
	}
	if !strings.HasPrefix(o.ConnAtExit, "pg connections:") {
		t.Errorf("ConnAtExit = %q, want a pg_stat_activity snapshot taken when the worker exited", o.ConnAtExit)
	}
	t.Logf("ended in %s: %s | %s", took.Round(time.Millisecond), o.WorkerExit, o.ConnAtExit)
}
