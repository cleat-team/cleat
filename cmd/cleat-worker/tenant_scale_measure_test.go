//go:build tenantscale

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3573 / cleat#3552. What a worker that serves N tenants costs, measured
// rather than argued: the reaper's per-tick cost, the rotating claim's per-tick
// cost, and how many dispatch ticks a NEW run waits before the rotation reaches
// its tenant. Not part of the default build: it needs a database and minutes.
//
//	go test -tags tenantscale ./cmd/cleat-worker/ -run TestTenantScale -count=1 -v \
//	  with CLEAT_TEST_POSTGRES / CLEAT_TEST_DB and/or CLEAT_TEST_MSSQL set, and
//	  TENANTSCALE_N=10,100,200  TENANTSCALE_K=16,32,64  TENANTSCALE_REAPS=15
//	  TENANTSCALE_TRIALS=30     TENANTSCALE_OUT=<file for the TENANTSCALE| rows>
//
// PostgreSQL needs pg_stat_statements loaded (shared_preload_libraries) and created
// in the database CLEAT_TEST_DB names; the harness fails, rather than skips, if it
// is missing. The projected first-notice figures round the mean tick count to a
// whole tick.
//
// It is written to be reused: every number it prints is a row beginning
// `TENANTSCALE|`, and a candidate that changes how a new run is noticed (a hint
// index, a NOTIFY payload) is compared by running the same file with that
// candidate wired into claimOneTick and reading the same rows.
//
// # What is measured and what is not
//
// MEASURED: wall time of w.reapOnce() and w.claimGeneral() against a real
// database as the RLS role, with N tenants registered and no work, cold (the
// first tick, which on SQL Server opens a pool per tenant) and warm; statements
// per reaper tick (PostgreSQL: transactions, which is what pg_stat_database
// counts; SQL Server: batch requests); and the number of claim ticks until a run
// started in a random tenant is claimed.
//
// PROJECTED, from those, and labelled so in the output: first-notice latency in
// seconds. It combines the measured tick count and tick cost with the dispatch
// loop's idle backoff, which is read from setup.go (sleep after the i-th empty
// tick is min(i, maxIdleTicks) * poll) and not re-run: a real dispatch loop
// executes what it claims. A worker that is already at maximum backoff, with
// nothing to wake it, waits out the rest of a maxIdleTicks*poll sleep before its
// first tick; one woken by PostgreSQL NOTIFY starts at once and backs off again
// from one poll.
//
// NOT MEASURED: several workers (their rotations interleave, so the figures are
// the one-worker worst case), a database on another host, and MySQL (single
// tenant by decision).

func scaleInts(t *testing.T, name string, def []int) []int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q: %q is not a positive integer", name, v, f)
		}
		out = append(out, n)
	}
	return out
}

// The first eight characters differ: the rigs name a tenant from them.
func scaleTenant(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000003573", i+0x1000) }

type scaleRows struct {
	t   *testing.T
	out *os.File
}

func (r *scaleRows) row(fields ...any) {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprint(f)
	}
	line := "TENANTSCALE|" + strings.Join(parts, "|")
	r.t.Log(line)
	if r.out != nil {
		fmt.Fprintln(r.out, line)
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func sortedDur(d []time.Duration) []time.Duration {
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c
}

func TestTenantScalePostgres(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("no PostgreSQL DSN set")
	}
	owner := testutil.SuiteTestDB(t, "cleat_worker_tenantscale")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	rlsDB.SetMaxOpenConns(40)
	rig := postgresHBRig(t, owner, rlsDB, true, scaleTenant(1))
	// Statements, counted by pg_stat_statements, which is exact and immediate.
	// pg_stat_database was tried first and is NOT: a pooled backend flushes its
	// counters lazily, so the delta read 5 statements for a ten-tenant reaper tick.
	// The extension must be loaded (shared_preload_libraries) and created in the
	// database CLEAT_TEST_DB names. It counts BEGIN and COMMIT as statements.
	var suiteDB string
	if err := owner.QueryRow(`SELECT current_database()`).Scan(&suiteDB); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_POSTGRES")
	}
	statsDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = statsDB.Close() })
	var calls int64
	if err := statsDB.QueryRow(`SELECT coalesce(sum(calls), 0) FROM pg_stat_statements`).Scan(&calls); err != nil {
		t.Fatalf("pg_stat_statements is not available (load it with shared_preload_libraries and CREATE EXTENSION it in the CLEAT_TEST_DB database): %v", err)
	}
	counter := func() int64 {
		var n int64
		if err := statsDB.QueryRow(
			`SELECT coalesce(sum(s.calls), 0) FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid WHERE d.datname = $1 AND s.query NOT LIKE '%pg_stat_statements%'`, suiteDB).Scan(&n); err != nil {
			t.Fatalf("reading pg_stat_statements: %v", err)
		}
		return n
	}
	runTenantScale(t, rig, "postgres", counter, true)
}

func TestTenantScaleSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	rig := mssqlHBRig(t, scaleTenant(1))
	counter := func() int64 {
		var n int64
		if err := rig.db.QueryRow(
			`SELECT cntr_value FROM sys.dm_os_performance_counters WHERE counter_name = 'Batch Requests/sec' AND object_name LIKE '%SQL Statistics%'`).Scan(&n); err != nil {
			t.Fatalf("reading Batch Requests: %v", err)
		}
		return n
	}
	runTenantScale(t, rig, "sqlserver", counter, false)
}

func runTenantScale(t *testing.T, rig *hbRig, dialect string, statements func() int64, hasNotify bool) {
	ctx := context.Background()
	ns := scaleInts(t, "TENANTSCALE_N", []int{10, 100, 200})
	ks := scaleInts(t, "TENANTSCALE_K", []int{16, 32, 64})
	reaps := scaleInts(t, "TENANTSCALE_REAPS", []int{15})[0]
	trials := scaleInts(t, "TENANTSCALE_TRIALS", []int{30})[0]
	rows := &scaleRows{t: t}
	if p := os.Getenv("TENANTSCALE_OUT"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		rows.out = f
	}
	rng := rand.New(rand.NewSource(3573))

	w := newTestWorkerFromStore(rig.ownStore)
	defer w.cancel()
	w.id = "tenantscale-worker"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = rig.factory
	w.claimAcrossTenants = true
	// The production defaults (--heartbeat, --max-reclaim-per-tick, --poll). The
	// test worker's own are millisecond-scale.
	w.heartbeatInterval = 5 * time.Second
	w.maxReclaimPerTick = 200
	w.pollInterval = 500 * time.Millisecond
	seedRecentlyConfirmedHealthy(w)

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	registered := 0
	deployed := map[string]bool{}
	for _, n := range ns {
		for registered < n {
			registered++
			rig.register(t, scaleTenant(registered))
		}
		listed, err := w.listTenantsToServe()
		if err != nil {
			t.Fatalf("listing tenants: %v", err)
		}
		// PRECONDITION: the worker sees the N tenants this row says, plus the default
		// one. A row measured against another population would be a different row.
		if len(listed) < n {
			t.Fatalf("the worker lists %d tenants with %d registered", len(listed), n)
		}

		// --- the reaper -----------------------------------------------------
		var cold time.Duration
		var warm []time.Duration
		before := statements()
		for i := 0; i < reaps; i++ {
			seedRecentlyConfirmedHealthy(w)
			start := time.Now()
			w.reapOnce()
			d := time.Since(start)
			if i == 0 {
				cold = d
			} else {
				warm = append(warm, d)
			}
		}
		perTick := float64(statements()-before) / float64(reaps)
		sw := sortedDur(warm)
		rows.row("reaper", dialect, "N="+strconv.Itoa(n), "ticks="+strconv.Itoa(reaps),
			"cold_ms="+fmt.Sprint(cold.Milliseconds()),
			"warm_p50_ms="+fmt.Sprint(pct(sw, 0.5).Milliseconds()),
			"warm_p95_ms="+fmt.Sprint(pct(sw, 0.95).Milliseconds()),
			"warm_max_ms="+fmt.Sprint(pct(sw, 1).Milliseconds()),
			fmt.Sprintf("statements_per_tick=%.1f", perTick),
			fmt.Sprintf("per_tenant=%.2f", perTick/float64(n+1)))

		// --- the rotating claim, per k --------------------------------------
		for _, k := range ks {
			w.claimTenantsPerTick = k
			var costs []time.Duration
			var coldClaim time.Duration
			before := statements()
			const claimTicks = 12
			for i := 0; i < claimTicks; i++ {
				start := time.Now()
				if _, err := w.claimGeneral(20); err != nil {
					t.Fatalf("claimGeneral: %v", err)
				}
				d := time.Since(start)
				if i == 0 {
					coldClaim = d
				} else {
					costs = append(costs, d)
				}
			}
			claimStmts := float64(statements()-before) / claimTicks
			sc := sortedDur(costs)
			tickCost := pct(sc, 0.5)
			rows.row("claim_tick", dialect, "N="+strconv.Itoa(n), "k="+strconv.Itoa(k),
				"cold_ms="+fmt.Sprint(coldClaim.Milliseconds()),
				"warm_p50_ms="+fmt.Sprint(tickCost.Milliseconds()),
				"warm_p95_ms="+fmt.Sprint(pct(sc, 0.95).Milliseconds()),
				fmt.Sprintf("statements_per_tick=%.1f", claimStmts))

			// --- first notice: ticks until a new run is claimed --------------
			var ticksTo []int
			for trial := 0; trial < trials; trial++ {
				tenant := scaleTenant(1 + rng.Intn(n))
				st, closeSt, err := rig.factory.OpenStore(ctx, tenant, "default")
				if err != nil {
					t.Fatalf("opening tenant store: %v", err)
				}
				defName := "scale-3573"
				if !deployed[tenant] {
					if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
						Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
						ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
					}); err != nil {
						t.Fatalf("DeployWorkflowDef: %v", err)
					}
					deployed[tenant] = true
				}
				wfID := fmt.Sprintf("scale-3573-%d-%d", n, time.Now().UnixNano())
				if _, _, err := st.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
					json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`), "", tenant, 0); err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				closeSt.Close()
				ticks, found := 0, false
				for ticks < 2*(n/k+2) {
					ticks++
					wfs, err := w.claimGeneral(20)
					if err != nil {
						t.Fatalf("claimGeneral: %v", err)
					}
					for _, wf := range wfs {
						if wf.ID == wfID {
							found = true
						}
					}
					if found {
						break
					}
				}
				if !found {
					t.Fatalf("a run started in tenant %s was not claimed in %d ticks at N=%d k=%d: the rotation does not cover the list", tenant, ticks, n, k)
				}
				ticksTo = append(ticksTo, ticks)
			}
			sort.Ints(ticksTo)
			mean := 0.0
			for _, x := range ticksTo {
				mean += float64(x)
			}
			mean /= float64(len(ticksTo))
			maxT := ticksTo[len(ticksTo)-1]
			rows.row("ticks_to_claim", dialect, "N="+strconv.Itoa(n), "k="+strconv.Itoa(k),
				"trials="+strconv.Itoa(trials),
				fmt.Sprintf("mean=%.1f", mean), "p50="+strconv.Itoa(ticksTo[len(ticksTo)/2]), "max="+strconv.Itoa(maxT),
				"arithmetic_max="+strconv.Itoa((n+1+k-1)/k))

			// PROJECTION (not measured): see the file's header.
			poll := w.pollInterval
			sleepAfter := func(i int) time.Duration { return time.Duration(min(i, maxIdleTicks)) * poll }
			idleFirst := func(m int, wake bool) time.Duration {
				// Time from the run's creation to the m-th tick.
				var d time.Duration
				if !wake {
					d += maxIdleTicks * poll / 2 // mean wait into the current max-backoff sleep
					for i := 1; i < m; i++ {
						d += maxIdleTicks * poll
					}
				} else {
					for i := 1; i < m; i++ {
						d += sleepAfter(i)
					}
				}
				return d + time.Duration(m)*tickCost
			}
			rows.row("first_notice_projected_s", dialect, "N="+strconv.Itoa(n), "k="+strconv.Itoa(k),
				fmt.Sprintf("idle_no_wake_mean=%.1f", idleFirst(int(mean+0.5), false).Seconds()),
				fmt.Sprintf("idle_no_wake_worst=%.1f", (idleFirst(maxT, false)+maxIdleTicks*poll/2).Seconds()),
				fmt.Sprintf("after_notify_mean=%.1f", idleFirst(int(mean+0.5), true).Seconds()),
				fmt.Sprintf("after_notify_worst=%.1f", idleFirst(maxT, true).Seconds()),
				"notify_applies="+fmt.Sprint(hasNotify))
		}
	}
}
