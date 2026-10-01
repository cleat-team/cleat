// TestConcurrentMergesToABrandNewKeyRaceWithoutHoldlock is cleat#2890's
// falsification, at the level that actually reproduces it.
//
// TestConcurrentPutsToABrandNewKeyDoNotRace (same package) drives n=60
// concurrent PUTs through the real HTTP handler and p.db -- the realistic,
// production-shaped path -- and never reproduced the race in measurement
// (15 rounds, 0 failures). That is not evidence the hazard is absent: each
// PUT through SQLDBAdapter's tenant-tx pays two extra round trips before its
// MERGE (BEGIN TRAN, then EXEC sp_set_session_context), and the resulting
// per-goroutine jitter is enough to mostly miss SQL Server's sub-millisecond
// "both sessions evaluated WHEN NOT MATCHED before either committed its
// INSERT" window, even synchronized behind a start barrier.
//
// This test removes that jitter without changing the statement under test:
// each goroutine pins one *sql.Conn, sets its session context ONCE before
// the barrier (so the MERGE's RLS-filtered MATCHED check behaves exactly as
// it would in production -- an unset session context would hide every
// existing row and make the race trivial/unrepresentative, not prove
// anything about the real hazard), and then the ONLY thing that crosses the
// barrier is upsertKV's own MSSQL statement, unmodified, via
// plugintest.ExecRebound (a raw *sql.Conn may not call plugin.Rebind
// directly -- scripts/check-no-raw-rebind.py).
//
// Measured standalone before this test was written (not committed; a
// throwaway probe against a disposable table with this exact MERGE text):
// 30 rounds of 40 concurrent racers, 80 of 1200 got SQL Server error 2627
// (duplicate key) without WITH (HOLDLOCK), 0 of 1200 with it. This test is
// the in-tree version of that probe, against the real kv_store table and
// the real upsertKV query text, so a future edit to the query is covered
// rather than a copy of it.
package kvstore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestConcurrentMergesToABrandNewKeyRaceWithoutHoldlock(t *testing.T) {
	if testing.Short() {
		t.Skip("holds many real connections open under a start barrier; skipping in -short")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)

	ctx := context.Background()
	dialect := plugin.Dialect(string(testutil.DialectMSSQL))
	p := &Plugin{dialect: dialect, logger: slog.Default(), config: Config{MaxValueSize: 1 << 20}}
	if err := plugin.RunMigrations(ctx, db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	const rounds = 20
	const n = 40

	var totalOK, totalDupKey, totalOtherErr int

	for round := 0; round < rounds; round++ {
		key := fmt.Sprintf("race-key-round-%d", round)

		var ready sync.WaitGroup
		start := make(chan struct{})
		var wg sync.WaitGroup
		ready.Add(n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := db.Conn(ctx)
				if err != nil {
					errs[i] = fmt.Errorf("db.Conn: %w", err)
					ready.Done()
					return
				}
				defer conn.Close()

				// Set once, before the barrier: this is what every PUT pays
				// for exactly once in production too (per-call there, since
				// SQLDBAdapter does not pin a connection across calls, but
				// the effect on THIS statement's RLS-filtered MATCHED
				// evaluation is identical either way). Only the MERGE itself
				// needs to be tightly synchronized to hit the race.
				if _, err := conn.ExecContext(ctx,
					`EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`,
					testTenantID.String()); err != nil {
					errs[i] = fmt.Errorf("sp_set_session_context: %w", err)
					ready.Done()
					return
				}

				ready.Done()
				<-start

				_, err = plugintest.ExecRebound(t, ctx, conn, dialect, upsertKV.For(dialect),
					testTenantID, key, plugin.JSONColumn{Raw: []byte(fmt.Sprintf(`{"writer":%d}`, i))})
				errs[i] = err
			}(i)
		}
		ready.Wait()
		close(start)
		wg.Wait()

		var ok, dup, other int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case strings.Contains(err.Error(), "2627") || strings.Contains(err.Error(), "duplicate key"):
				dup++
			default:
				other++
				t.Errorf("round %d: unexpected error (not a 2627 duplicate-key): %v", round, err)
			}
		}
		totalOK += ok
		totalDupKey += dup
		totalOtherErr += other
	}

	t.Logf("across %d rounds of %d concurrent MERGEs to a brand-new key: ok=%d dupkey=%d other=%d",
		rounds, n, totalOK, totalDupKey, totalOtherErr)

	if totalDupKey != 0 {
		t.Errorf("%d of %d concurrent MERGEs to a brand-new key got a duplicate-key error (2627) -- "+
			"two racers both evaluated \"WHEN NOT MATCHED\" before either committed its INSERT. "+
			"cleat#2890: upsertKV's MSSQL MERGE needs WITH (HOLDLOCK), the same fix cleat#2268 "+
			"applied to eventstore's upsertStreamHead", totalDupKey, rounds*n)
	}
}
