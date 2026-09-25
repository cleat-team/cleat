package auditlog

// Retention and the chain (cleat#2047): what it removes is a recorded prefix, the chain
// still verifies from the floor, and it never turns a gap into a recorded fact.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// tsOf reads a row's timestamp as epoch microseconds, the way the chain does.
func (e *chainEnv) tsOf(tenant uuid.UUID, seq int64) time.Time {
	e.t.Helper()
	var us int64
	e.scan(tenant, fmt.Sprintf(`SELECT %s FROM audit_events WHERE tenant_id = $1 AND seq = $2`,
		epochMicrosExpr(e.d.dialect, "timestamp")), []any{tenant.String(), seq}, &us)
	return time.UnixMicro(us).UTC()
}

func (e *chainEnv) rowCount(tenant uuid.UUID) int {
	e.t.Helper()
	var n int
	e.scan(tenant, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1`, []any{tenant.String()}, &n)
	return n
}

func (e *chainEnv) mustVerifyOK(tenant uuid.UUID, what string) ChainReport {
	e.t.Helper()
	rep := e.verify(tenant)
	if !rep.OK() {
		e.t.Fatalf("%s: the chain does not verify: %+v (break %+v)", what, rep, rep.Break)
	}
	return rep
}

func TestRetentionRemovesAPrefixAndRecordsTheFloor(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		errs := p.captureErrors()
		tenant := uuid.New()
		e.record(p, tenant, 20)
		cutoff := e.tsOf(tenant, 13) // rows 1..12 are older than this, 13 is not

		n, err := p.retainTenant(context.Background(), tenant, cutoff)
		if err != nil || n != 12 {
			t.Fatalf("retainTenant removed %d rows, %v; want 12", n, err)
		}
		rep := e.mustVerifyOK(tenant, "after retention")
		if rep.FloorSeq != 12 || rep.Checked != 8 || rep.HeadSeq != 20 {
			t.Fatalf("after removing the first 12 of 20: %+v, want floor 12, 8 checked, head 20", rep)
		}
		if got := e.rowCount(tenant); got != 8 {
			t.Fatalf("%d rows remain, want 8", got)
		}

		// The chain carries on from where it was, not from the floor.
		e.record(p, tenant, 3)
		if rep := e.mustVerifyOK(tenant, "after appending past the floor"); rep.Checked != 11 || rep.HeadSeq != 23 || rep.FloorSeq != 12 {
			t.Fatalf("%+v", rep)
		}

		// A second sweep with the same cutoff has nothing to do.
		if n, err := p.retainTenant(context.Background(), tenant, cutoff); err != nil || n != 0 {
			t.Fatalf("a repeat sweep removed %d rows, %v; want 0", n, err)
		}
		if len(*errs) > 0 {
			t.Fatalf("logged: %s", (*errs)[0])
		}
	})
}

// Everything expired: the chain is empty and its head remembers where it was, so the next
// event links to the last row that existed rather than starting a second chain.
func TestRetentionOfEveryRowLeavesAChainThatCanBeExtended(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 6)
		if n, err := p.retainTenant(context.Background(), tenant, time.Now().Add(time.Hour)); err != nil || n != 6 {
			t.Fatalf("removed %d, %v; want 6", n, err)
		}
		rep := e.mustVerifyOK(tenant, "after removing everything")
		if rep.Checked != 0 || rep.FloorSeq != 6 || rep.HeadSeq != 6 {
			t.Fatalf("%+v", rep)
		}
		e.record(p, tenant, 2)
		if rep := e.mustVerifyOK(tenant, "after extending an emptied chain"); rep.Checked != 2 || rep.HeadSeq != 8 || rep.FloorSeq != 6 {
			t.Fatalf("%+v", rep)
		}
	})
}

func TestRetentionRemovesAtMostOneBatchPerSweep(t *testing.T) {
	old := retentionBatch
	retentionBatch = 4
	t.Cleanup(func() { retentionBatch = old })
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 10)
		future := time.Now().Add(time.Hour)
		for i, want := range []int64{4, 4, 2, 0} {
			n, err := p.retainTenant(context.Background(), tenant, future)
			if err != nil || n != want {
				t.Fatalf("sweep %d removed %d, %v; want %d", i+1, n, err, want)
			}
			e.mustVerifyOK(tenant, fmt.Sprintf("after sweep %d", i+1))
		}
	})
}

// THE SAFEGUARD. If rows in the expired prefix are already missing, moving the floor over
// them would turn a deletion into a recorded fact. Retention must leave the chain alone
// and let verify report it.
func TestRetentionDoesNotMoveTheFloorOverAGap(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 20)
		e.mustChange(tenant, `DELETE FROM audit_events WHERE tenant_id = $1 AND seq = 5`, tenant.String())
		cutoff := e.tsOf(tenant, 13)

		n, err := p.retainTenant(context.Background(), tenant, cutoff)
		if err != nil || n != 0 {
			t.Fatalf("retention removed %d rows, %v; want 0 -- it must not act on a chain with a gap in the prefix", n, err)
		}
		rep := e.verify(tenant)
		if rep.OK() || rep.Break.Kind != BreakMissing || rep.Break.Seq != 5 || rep.FloorSeq != 0 {
			t.Fatalf("after a refused retention the chain reports %+v (break %+v), want missing at seq 5 with the floor unmoved", rep, rep.Break)
		}
		if got := e.rowCount(tenant); got != 19 {
			t.Fatalf("%d rows remain, want the 19 that were there", got)
		}
	})
}

// The row that would become the new floor is itself gone. There is no hash to record, so
// retention must leave the tenant alone -- and say so as a skip, not as an error: an error
// here would fail every hourly sweep for that tenant, indefinitely, over something the
// verifier already reports and an operator has to decide.
func TestRetentionSkipsATenantWhoseNewFloorRowIsGone(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 20)
		cutoff := e.tsOf(tenant, 13) // rows 1..12 are expired, so 12 would be the new floor
		e.mustChange(tenant, `DELETE FROM audit_events WHERE tenant_id = $1 AND seq = 12`, tenant.String())

		n, err := p.retainTenant(context.Background(), tenant, cutoff)
		if err != nil || n != 0 {
			t.Fatalf("retention removed %d rows, err %v; want 0 and NO error for a chain whose new-floor row is gone", n, err)
		}
		rep := e.verify(tenant)
		if rep.OK() || rep.Break.Kind != BreakMissing || rep.Break.Seq != 12 || rep.FloorSeq != 0 {
			t.Fatalf("after the skipped sweep the chain reports %+v (break %+v), want missing at seq 12 with the floor unmoved", rep, rep.Break)
		}
		if got := e.rowCount(tenant); got != 19 {
			t.Fatalf("%d rows remain, want the 19 that were there", got)
		}
	})
}

// A floor cannot be talked into place: a prefix removed by hand, or a floor edited to fit,
// is reported.
func TestAFloorThatDoesNotMatchTheRowsIsReported(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		T := tenant.String()
		e.record(p, tenant, 12)
		if n, err := p.retainTenant(context.Background(), tenant, e.tsOf(tenant, 7)); err != nil || n != 6 {
			t.Fatalf("setup: removed %d, %v", n, err)
		}
		e.mustVerifyOK(tenant, "the honest floor")

		// Lower the floor: the first surviving row is no longer the one after it.
		e.mustChange(tenant, `UPDATE audit_chain_heads SET floor_seq = 3 WHERE tenant_id = $1`, T)
		if rep := e.verify(tenant); rep.OK() || rep.Break.Kind != BreakMissing || rep.Break.Seq != 4 {
			t.Fatalf("a lowered floor: %+v %+v", rep, rep.Break)
		}
		// Right seq, wrong hash: the first row does not link to the floor.
		e.mustChange(tenant, `UPDATE audit_chain_heads SET floor_seq = 6, floor_hash = $1 WHERE tenant_id = $2`, strings.Repeat("9a", 32), T)
		if rep := e.verify(tenant); rep.OK() || rep.Break.Kind != BreakRelinked || rep.Break.Seq != 7 {
			t.Fatalf("a floor hash that is not the removed row's: %+v %+v", rep, rep.Break)
		}
	})
}

// The whole sweep, through the tenant loop: every tenant is visited, a tenant with no
// chain is fine, and rows written before the chain existed (no seq) are still removed on
// their age.
func TestTheRetentionSweepVisitsEveryTenantAndTheUnchainedRows(t *testing.T) {
	forEachIsolatedChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		errs := p.captureErrors()
		// NONE OF THEM IS REGISTERED in the tenants table, deliberately: audit rows outlive
		// their tenant (there is no foreign key, and drop-tenant leaves them), and a sweep
		// that enumerated the registry would keep a removed tenant's rows forever.
		a := uuid.New()
		tenants := []uuid.UUID{a, uuid.New(), uuid.New()}
		e.record(p, a, 5)
		chained := int64(5)
		floors := map[uuid.UUID]int64{a: 5}
		e.record(p, tenants[1], 4) // tenants[2] never gets a chain
		chained += 4
		floors[tenants[1]] = 4

		// Rows from before the chain: no seq, one old and one recent.
		old := map[plugin.Dialect]string{
			plugin.DialectPostgres: `now() - interval '200 days'`,
			plugin.DialectMySQL:    `NOW(6) - INTERVAL 200 DAY`,
			plugin.DialectMSSQL:    `DATEADD(DAY, -200, SYSDATETIMEOFFSET())`,
		}[e.d.dialect]
		recent := map[plugin.Dialect]string{
			plugin.DialectPostgres: `now()`,
			plugin.DialectMySQL:    `NOW(6)`,
			plugin.DialectMSSQL:    `SYSDATETIMEOFFSET()`,
		}[e.d.dialect]
		for _, ts := range []string{old, recent} {
			e.mustChange(a, `INSERT INTO audit_events (id, tenant_id, timestamp, method, path) VALUES ($1, $2, `+ts+`, 'GET', '/legacy')`,
				uuid.NewString(), a.String())
		}

		// Now is 100 days on: the chained rows are recent enough to keep at 90 days
		// retention only if the clock is left alone, so first prove the sweep keeps them.
		if n, err := p.cleanupRetention(context.Background()); err != nil {
			t.Fatalf("cleanupRetention: %v", err)
		} else if n != 1 {
			t.Fatalf("with the clock unmoved the sweep removed %d rows, want only the one 200-day-old unchained row", n)
		}
		if rep := e.mustVerifyOK(a, "tenant a after a sweep that kept its rows"); rep.Checked != 5 || rep.Unchained != 1 {
			t.Fatalf("%+v: want 5 chained rows kept and 1 unchained (the recent one)", rep)
		}

		// Then move the clock past retention: every chained row of every tenant goes,
		// each chain verifies, and the tenant with no chain is no problem.
		p.now = func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }
		if n, err := p.cleanupRetention(context.Background()); err != nil || n != chained+1 {
			t.Fatalf("with the clock moved on the sweep removed %d rows, %v; want %d", n, err, chained+1)
		}
		for tn, want := range floors {
			rep := e.mustVerifyOK(tn, "after everything expired")
			if rep.Checked != 0 || rep.FloorSeq != want || rep.Unchained != 0 {
				t.Fatalf("%+v", rep)
			}
		}
		if len(*errs) > 0 {
			t.Fatalf("logged: %s", (*errs)[0])
		}
	})
}
