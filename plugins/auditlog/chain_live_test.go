package auditlog

// The chain while it is being written and swept, and the ways a floor can lie
// (cleat#2047, review of the first version).
//
// The first version of VerifyChain read the head, then paged the rows in separate queries,
// with no bound. Probed with a continuous writer it reported a false break on PostgreSQL
// 25 runs in 30, MySQL 4 in 30 and SQL Server 30 in 30, and during a retention sweep a
// false `missing` on PostgreSQL 15 in 30 and a deadlock victim on SQL Server. A verifier
// that cries wolf on a healthy log is worse than none: operators learn to ignore it.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// writeUntil appends to tenant's chain from its own worker until stop is closed.
func (e *chainEnv) writeUntil(stop <-chan struct{}, tenant uuid.UUID) (*sync.WaitGroup, *atomic.Int64) {
	var wg sync.WaitGroup
	var n atomic.Int64
	w := e.plugin()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			w.recordAudit(context.Background(), tenant, "writer", "GET", fmt.Sprintf("/live/%d", i), 200, "10.0.0.1", "agent", time.Millisecond)
			n.Add(1)
		}
	}()
	return &wg, &n
}

func TestVerifyOfAChainBeingAppendedToReportsNoBreak(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		tenant := uuid.New()
		e.record(e.plugin(), tenant, 20)
		stop := make(chan struct{})
		wg, written := e.writeUntil(stop, tenant)
		defer func() { close(stop); wg.Wait() }()

		p := e.plugin()
		var falses []string
		// At least 40 verifications, and until enough of them OVERLAPPED the writer: the count that
		// matters is verifications during which the head moved, not rows appended since the start,
		// because a starved writer can have written a batch early and then stalled (cleat#2208 saw 2
		// rows). The verifications keep running while it catches up. The deadline keeps a wedged
		// writer from hanging the run and names what was seen.
		const wantMoving = 10
		deadline := time.Now().Add(90 * time.Second)
		runs, moving := 0, 0
		for ; runs < 40 || moving < wantMoving; runs++ {
			if time.Now().After(deadline) {
				t.Fatalf("UNMEASURED: after %d verifications only %d overlapped a writer that was appending (%d rows in all), want %d; the chain was not moving", runs, moving, written.Load(), wantMoving)
			}
			before := written.Load()
			rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
			if err != nil {
				t.Fatalf("verify %d: %v", runs, err)
			}
			if written.Load() != before {
				moving++
			}
			if !rep.OK() {
				falses = append(falses, fmt.Sprintf("%s at seq %d (%s)", rep.Break.Kind, rep.Break.Seq, rep.Break.Detail))
			}
		}
		if len(falses) > 0 {
			t.Fatalf("%d of %d verifications of a healthy, live chain reported a break; the first: %s", len(falses), runs, falses[0])
		}
	})
}

func TestVerifyDuringARetentionSweepReportsNoBreak(t *testing.T) {
	forEachIsolatedChainDialect(t, func(t *testing.T, e *chainEnv) {
		tenant := uuid.New()
		sweeper := e.plugin()
		// Every row is expired as far as the sweeper's clock is concerned, and it removes a
		// few at a time, so the floor moves continuously while the writer appends.
		sweeper.now = func() time.Time { return time.Now().Add(400 * 24 * time.Hour) }
		old := retentionBatch
		retentionBatch = 3
		t.Cleanup(func() { retentionBatch = old })

		e.record(e.plugin(), tenant, 30)
		stop := make(chan struct{})
		wg, written := e.writeUntil(stop, tenant)
		var swept atomic.Int64
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A sweep can lose a deadlock to the writer: the next one repeats it.
				if n, err := sweeper.cleanupRetention(context.Background()); err == nil {
					swept.Add(n)
				}
				// Sweeps are hourly in production. This is a hostile pace on purpose (the floor moves
				// every ~70ms, many times per verification), and still one a verifier keeps up with.
				time.Sleep(60 * time.Millisecond)
			}
		}()
		defer func() { close(stop); wg.Wait() }()

		p := e.plugin()
		var falses []string
		// At least 40 verifications, and until the floor and the head have both been seen
		// moving: a fixed count can finish before a slow sweeper has done a batch, and then
		// it has measured nothing. The deadline keeps a wedged sweeper from hanging the run.
		deadline := time.Now().Add(90 * time.Second)
		runs := 0
		for ; runs < 40 || swept.Load() < 6 || written.Load() < 5; runs++ {
			if time.Now().After(deadline) {
				t.Fatalf("UNMEASURED: after %d verifications the sweeper had removed %d rows and the writer added %d; the floor and the head were not both moving", runs, swept.Load(), written.Load())
			}
			rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
			if err != nil {
				t.Fatalf("verify %d: %v", runs, err)
			}
			if !rep.OK() {
				falses = append(falses, fmt.Sprintf("%s at seq %d (%s)", rep.Break.Kind, rep.Break.Seq, rep.Break.Detail))
			}
		}
		if len(falses) > 0 {
			t.Fatalf("%d of %d verifications during a retention sweep reported a break; the first: %s", len(falses), runs, falses[0])
		}
	})
}

// A floor cannot be talked forward by something other than retention and pass for it,
// provided the verifier is told the retention period. Without that it cannot tell, and the
// documentation says so; that limit is pinned here too.
func TestAFloorOverUnexpiredRowsIsReportedWhenTheRetentionIsKnown(t *testing.T) {
	forEachIsolatedChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 10)
		T := tenant.String()
		var h6 string
		e.scan(tenant, `SELECT row_hash FROM audit_events WHERE tenant_id = $1 AND seq = 6`, []any{T}, &h6)
		ts6 := e.tsOf(tenant, 6).UnixMicro()

		// The forgery: delete the first six rows and move the floor over them, recording
		// the true timestamp of row 6 (which is minutes old). It verifies as a chain.
		e.mustChange(tenant, `DELETE FROM audit_events WHERE tenant_id = $1 AND seq <= 6`, T)
		e.mustChange(tenant, `UPDATE audit_chain_heads SET floor_seq = 6, floor_hash = $1, floor_ts = $2 WHERE tenant_id = $3`, strings.TrimSpace(h6), ts6, T)

		opts := VerifyOptions{RetentionDays: 90}
		rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, opts)
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK() || rep.Break.Kind != BreakFloorUnexpired || rep.Break.Seq != 6 {
			t.Fatalf("a floor over rows minutes old, retention 90 days: %+v (break %+v), want %s at seq 6", rep, rep.Break, BreakFloorUnexpired)
		}
		// The stated limit: without the retention period the same state verifies.
		rep, err = VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
		if err != nil || !rep.OK() || rep.Checked != 4 {
			t.Fatalf("with no retention period given: %+v %v; the chain itself is intact from the floor, so this must verify", rep, err)
		}

		// A floor written by retention itself is fine, judged at a later date.
		honest := uuid.New()
		sweeper := e.plugin()
		sweeper.now = func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }
		e.record(p, honest, 10)
		if n, err := sweeper.cleanupRetention(context.Background()); err != nil || n < 10 {
			t.Fatalf("the sweep removed %d rows, %v", n, err)
		}
		rep, err = VerifyChain(context.Background(), p.db, e.d.dialect, honest, VerifyOptions{
			RetentionDays: 90, Now: func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }})
		if err != nil || !rep.OK() || rep.FloorSeq < 10 {
			t.Fatalf("a floor recorded by retention, verified a hundred days on: %+v %v (break %+v)", rep, err, rep.Break)
		}
		// A floor with no recorded timestamp cannot be shown to cover only expired rows.
		e.mustChange(honest, `UPDATE audit_chain_heads SET floor_ts = 0 WHERE tenant_id = $1`, honest.String())
		rep, _ = VerifyChain(context.Background(), p.db, e.d.dialect, honest, VerifyOptions{RetentionDays: 90})
		if rep.OK() || rep.Break.Kind != BreakFloorUnexpired {
			t.Fatalf("a floor with no timestamp: %+v", rep)
		}
	})
}

// A path is chosen by the caller. On MySQL and SQL Server an over-long one failed the
// insert -- logged, not retried -- so the request was not in the log at all.
func TestAnOverLongValueIsTruncatedNotDropped(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		errs := p.captureErrors()
		tenant := uuid.New()
		long := "/" + strings.Repeat("a", 1004)           // 1005 characters
		astral := "/" + strings.Repeat("\U0001F600", 600) // 601 characters, 1201 UTF-16 units
		p.recordAudit(context.Background(), tenant, "u", "GET", long, 200, "1.1.1.1", strings.Repeat("x", 100000), time.Millisecond)
		p.recordAudit(context.Background(), tenant, "u", "GET", astral, 200, "1.1.1.1", "ua", time.Millisecond)
		p.recordAudit(context.Background(), tenant, "u", strings.Repeat("M", 300), "/ok", 200, "1.1.1.1", "ua", time.Millisecond)
		if len(*errs) > 0 {
			t.Fatalf("an over-long value was not recorded: %s", (*errs)[0])
		}
		if rep := e.verify(tenant); !rep.OK() || rep.Checked != 3 {
			t.Fatalf("%+v (break %+v), want the 3 rows recorded and verified", rep, rep.Break)
		}
		var path, ua string
		e.scan(tenant, `SELECT path, user_agent FROM audit_events WHERE tenant_id = $1 AND seq = 1`, []any{tenant.String()}, &path, &ua)
		if !strings.HasSuffix(path, truncationMarker) || len([]rune(path)) > 700 || !strings.HasSuffix(ua, truncationMarker) {
			t.Errorf("path %d runes ending %q, user agent ending %q: want both cut, with the marker", len([]rune(path)), path[len(path)-20:], ua[len(ua)-20:])
		}
	})
}

func TestFitTextRespectsBothDialectsLimits(t *testing.T) {
	for _, c := range []struct {
		name        string
		in          string
		runes, unit int
		cut         bool
	}{
		{"short", "/ok", 700, 800, false},
		{"exactly at the limit", strings.Repeat("a", 700), 700, 800, false},
		{"one over in characters", strings.Repeat("a", 701), 700, 800, true},
		{"within characters, over in UTF-16 units", strings.Repeat("\U0001F600", 500), 700, 800, true},
	} {
		got := fitText(c.in, c.runes, c.unit)
		units := 0
		for _, r := range got {
			units++
			if r > 0xFFFF {
				units++
			}
		}
		if len([]rune(got)) > c.runes || units > c.unit {
			t.Errorf("%s: %d characters, %d units, over %d/%d", c.name, len([]rune(got)), units, c.runes, c.unit)
		}
		if c.cut != strings.HasSuffix(got, truncationMarker) || (!c.cut && got != c.in) {
			t.Errorf("%s: cut=%v but result ends %q", c.name, c.cut, got[max(0, len(got)-16):])
		}
	}
	// Never splits a character.
	if got := fitText(strings.Repeat("é", 900), 700, 900); strings.ContainsRune(got, '�') {
		t.Errorf("a character was split: %q", got[:20])
	}
}

// The classifier decides which failures a verification repeats. It is matched on message
// text because the three drivers share no error type, so it is pinned against the
// messages each really produces (copied from measured runs), and against ones that must
// NOT be repeated.
func TestOnlyADeadlockVictimIsRepeated(t *testing.T) {
	for msg, want := range map[string]bool{
		"pq: deadlock detected (40P01)":                                                          true,
		"pq: could not serialize access due to concurrent update (40001)":                        true,
		"Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction": true,
		"mssql: Transaction (Process ID 55) was deadlocked on lock resources with another process and has been chosen as the deadlock victim. Rerun the transaction. (1205)": true,
		"pq: relation \"audit_chain_heads\" does not exist (42P01)": false,
		"pq: permission denied for table audit_events (42501)":      false,
		"context deadline exceeded":                                 false,
	} {
		if got := isTransientDBError(fmt.Errorf("%s", msg)); got != want {
			t.Errorf("isTransientDBError(%q) = %v, want %v", msg, got, want)
		}
	}
}

// hookDB runs fn before the n-th query.
type hookDB struct {
	plugin.PluginDB
	n, at int
	fn    func()
}

func (h *hookDB) Query(ctx context.Context, q string, a ...any) (plugin.Rows, error) {
	if h.n++; h.n == h.at {
		h.fn()
	}
	return h.PluginDB.Query(ctx, q, a...)
}

// The deterministic form of the race above: a sweep removes the first rows AFTER verify
// read the head and BEFORE it reads any row. The scan then finds row 6 where the old floor
// says row 1 should be -- a gap that is an artefact of the sweep, not a break. It must be
// repeated from the new floor and come out clean; and a real gap must still be reported.
func TestAGapCreatedByASweepDuringVerifyIsNotReported(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 12)
		cutoff := e.tsOf(tenant, 6).Add(time.Microsecond) // rows 1..6 are expired
		sweeper := e.plugin()

		v := e.plugin()
		v.db = &hookDB{PluginDB: v.db, at: 1, fn: func() {
			if n, err := sweeper.retainTenant(context.Background(), tenant, cutoff); err != nil || n != 6 {
				t.Errorf("the sweep removed %d rows, %v; want 6", n, err)
			}
		}}
		rep, err := VerifyChain(context.Background(), v.db, e.d.dialect, tenant, VerifyOptions{})
		if err != nil || !rep.OK() {
			t.Fatalf("verify across a sweep: %+v, %v (break %+v), want a clean chain", rep, err, rep.Break)
		}
		if rep.Checked != 6 || rep.FloorSeq != 6 {
			t.Errorf("verified %d rows from floor %d, want the 6 that remain, from the floor the sweep recorded", rep.Checked, rep.FloorSeq)
		}

		// A real gap above the floor is still a gap, however the floor moved.
		e.mustChange(tenant, `DELETE FROM audit_events WHERE tenant_id = $1 AND seq = 9`, tenant.String())
		rep, err = VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
		if err != nil || rep.OK() || rep.Break.Kind != BreakMissing || rep.Break.Seq != 9 {
			t.Fatalf("a real gap: %+v, %v, want missing at seq 9", rep, err)
		}
	})
}

// A tenant's first append creates its head. A verifier that read "no head" just before it,
// and then found rows, would call a healthy new chain headless.
func TestAHeadCreatedDuringVerifyIsNotReportedMissing(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		tenant := uuid.New()
		writer := e.plugin()
		v := e.plugin()
		v.db = &hookDB{PluginDB: v.db, at: 1, fn: func() { e.record(writer, tenant, 3) }}
		rep, err := VerifyChain(context.Background(), v.db, e.d.dialect, tenant, VerifyOptions{})
		if err != nil || !rep.OK() || rep.Checked != 3 {
			t.Fatalf("a chain whose first rows landed during verify: %+v, %v (break %+v), want 3 rows, clean", rep, err, rep.Break)
		}
		// And a headless chain that is not a race is still reported.
		orphan := uuid.New()
		e.record(writer, orphan, 2)
		e.mustChange(orphan, `DELETE FROM audit_chain_heads WHERE tenant_id = $1`, orphan.String())
		rep, err = VerifyChain(context.Background(), writer.db, e.d.dialect, orphan, VerifyOptions{})
		if err != nil || rep.OK() || rep.Break.Kind != BreakHeadMissing {
			t.Fatalf("rows with no head: %+v, %v, want %s", rep, err, BreakHeadMissing)
		}
	})
}

// A floor is trusted only if nothing survives below it: retention deletes the rows at or
// below the floor in the same transaction that moves it. One UPDATE of the head (floor_seq
// and the hash of the row there) used to hide an edit anywhere below that row, because the
// scan starts above the floor and never looked, whatever the retention period or the
// timestamp the forger wrote.
func TestRowsSurvivingBelowAFloorAreReported(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		T := tenant.String()
		e.record(p, tenant, 20)
		e.mustChange(tenant, `UPDATE audit_events SET path = '/edited' WHERE tenant_id = $1 AND seq = 5`, T)
		var h10 string
		e.scan(tenant, `SELECT row_hash FROM audit_events WHERE tenant_id = $1 AND seq = 10`, []any{T}, &h10)
		// The forgery: one UPDATE. Rows 1..10 stay where they are.
		e.mustChange(tenant, `UPDATE audit_chain_heads SET floor_seq = 10, floor_hash = $1, floor_ts = $2 WHERE tenant_id = $3`,
			strings.TrimSpace(h10), time.Now().Add(-100*24*time.Hour).UnixMicro(), T)

		for name, opts := range map[string]VerifyOptions{
			"no options":                         {},
			"retention 90 days, floor_ts forged": {RetentionDays: 90},
		} {
			rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, opts)
			if err != nil || rep.OK() || rep.Break.Kind != BreakRowsBelowFloor || rep.Break.Seq != 1 {
				t.Fatalf("%s: %+v, %v (break %+v), want %s at seq 1 -- the edit at seq 5 must not be hidden by the floor", name, rep, err, rep.Break, BreakRowsBelowFloor)
			}
			// Reporting the rows below the floor must not make the report claim the floor's age
			// was not checked when a retention period was supplied.
			if rep.FloorAgeChecked != (opts.RetentionDays > 0) {
				t.Fatalf("%s: floor_age_checked is %v, want %v", name, rep.FloorAgeChecked, opts.RetentionDays > 0)
			}
		}

		// The honest floor: retention removed the rows, so nothing is below it, and the report
		// says whether the floor's age was checked.
		honest := uuid.New()
		sweeper := e.plugin()
		sweeper.now = func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }
		e.record(p, honest, 10)
		e.record(p, honest, 4)
		if n, err := sweeper.retainTenant(context.Background(), honest, e.tsOf(honest, 10).Add(time.Microsecond)); err != nil || n != 10 {
			t.Fatalf("the sweep removed %d rows, %v; want 10", n, err)
		}
		rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, honest, VerifyOptions{})
		if err != nil || !rep.OK() || rep.FloorSeq != 10 || rep.FloorAgeChecked {
			t.Fatalf("an honest floor, no retention period: %+v, %v; want ok, floor 10, floor_age_checked false", rep, err)
		}
		rep, err = VerifyChain(context.Background(), p.db, e.d.dialect, honest, VerifyOptions{RetentionDays: 90, Now: func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }})
		if err != nil || !rep.OK() || !rep.FloorAgeChecked {
			t.Fatalf("an honest floor, retention period given: %+v, %v; want ok and floor_age_checked true", rep, err)
		}
	})
}
