// TestMixedWorkloadOfOldAndNewCodeWritersDoesNotFailNewAppends pins G4 from
// cleat-review + the coordinator, cleat#2268 round 2: appendOnce's one-shot
// resync (see its doc comment) clears a SINGLE old-binary collision, but a
// BUSY rolling-upgrade window -- real old-binary writers and real new-code
// appenders both hitting one stream at once -- produces failures a single
// resync attempt cannot clear:
//
//   - a different writer (old or new) claims the resync's own target value
//     in the gap between its MAX read and its insert, so the resync's
//     insert collides too;
//   - a deadlock or serialization failure aborts the transaction before any
//     resync logic runs at all.
//
// Neither is reachable by new-code-only load, which is why
// TestConcurrentAppendsGetContiguousSequences and
// TestOldStyleWriterBetweenAppendsIsAbsorbedByResync didn't catch it.
//
// This drives 10 real old-binary writers (develop's pre-cleat#2268 shape:
// read MAX(sequence), insert at MAX+1, its own 32-attempt isPKConflict
// retry loop, no event_stream_head involvement at all) concurrently against
// 10 real new-code appends through p.handleAppend, all racing one stream.
// Measured against 8ee51b63 (appendOnce's one-shot resync, no outer retry):
// postgres 6, 5 and 2 of 10 new-code appends failed across three rounds;
// mysql 10 of 10 failed every round (Error 1213 on upsertStreamHead itself,
// never retried); mssql 4, 3 and 4 of 10. Old-binary writers errored zero
// times in every round -- their own retry loop already covers the
// collisions THEY see.
//
// Recreated from cleat-review's scratch probe (cleat#2268 round 2 review);
// adapted to this repo's test conventions and to assert rather than log.
package eventstore

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestMixedWorkloadOfOldAndNewCodeWritersDoesNotFailNewAppends(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: slog.Default(), config: Config{MaxEventSize: 1 << 20}}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			streamID := "mixed-workload-test-" + uuid.New().String()

			appendNew := func() (code int, body string) {
				req := httptest.NewRequest("POST", "/events/"+streamID,
					strings.NewReader(`{"n":1}`)).WithContext(tenantCtx)
				req.SetPathValue("stream_id", streamID)
				rec := httptest.NewRecorder()
				p.handleAppend(rec, req)
				return rec.Code, rec.Body.String()
			}

			// develop's pre-cleat#2268 appendOnce: read MAX(sequence), insert
			// at MAX+1, no event_stream_head involvement -- the exact shape
			// an old binary still runs during a rolling upgrade.
			// No manual plugin.Rebind on any of tx's calls below: tx
			// (plugin.PluginTx, via engine.SQLDBAdapter) already calls
			// plugin.RebindArgs internally on every Exec/QueryRow -- same
			// route a_concurrent_appends_get_contiguous_sequences_test.go
			// uses on p.db. "No raw plugin.Rebind guard" forbids the
			// direct call in a _test.go file for exactly this reason: a
			// raw handle needs it (plugins/plugintest.ExecRebound), a
			// PluginDB/PluginTx handle already does it.
			oldOnce := func() error {
				tx, err := p.db.Begin(tenantCtx)
				if err != nil {
					return err
				}
				var m int64
				if err := tx.QueryRow(tenantCtx,
					`SELECT COALESCE(MAX(sequence), 0) FROM event_stream WHERE tenant_id = $1 AND stream_id = $2`,
					tenantID, streamID).Scan(&m); err != nil {
					tx.Rollback()
					return err
				}
				if _, err := tx.Exec(tenantCtx, insertEvent.For(dialect),
					tenantID, streamID, m+1, `{"old":1}`); err != nil {
					tx.Rollback()
					return err
				}
				return tx.Commit()
			}
			// develop's own retry loop shape around that, so an old binary's
			// OWN collisions with itself are already covered -- this test is
			// about what happens to the NEW-code side, not the old.
			appendOld := func() error {
				var err error
				for a := 0; a < 32; a++ {
					if err = oldOnce(); err == nil || !isPKConflict(err) {
						return err
					}
					time.Sleep(time.Duration(2+a) * time.Millisecond)
				}
				return err
			}

			const n = 10
			var wg sync.WaitGroup
			newCodes := make([]int, n)
			newBodies := make([]string, n)
			oldErrs := make([]error, n)
			for i := 0; i < n; i++ {
				wg.Add(2)
				go func(i int) {
					defer wg.Done()
					newCodes[i], newBodies[i] = appendNew()
				}(i)
				go func(i int) {
					defer wg.Done()
					oldErrs[i] = appendOld()
				}(i)
			}
			wg.Wait()

			newFailures, oldFailures := 0, 0
			for i, c := range newCodes {
				if c != 201 {
					newFailures++
					t.Logf("on %s: new-code append %d got %d: %s", be.Name, i, c, newBodies[i])
				}
			}
			for i, e := range oldErrs {
				if e != nil {
					oldFailures++
					t.Logf("on %s: old-binary write %d: %v", be.Name, i, e)
				}
			}

			var rowCount, maxSeq int64
			if err := p.db.QueryRow(tenantCtx,
				`SELECT COUNT(*), COALESCE(MAX(sequence), 0) FROM event_stream WHERE tenant_id = $1 AND stream_id = $2`,
				tenantID, streamID).Scan(&rowCount, &maxSeq); err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}

			followUpCode, followUpBody := appendNew()

			t.Logf("on %s: new-code failures=%d/%d old-binary failures=%d/%d rows=%d max_sequence=%d "+
				"contiguous=%v follow-up=%d", be.Name, newFailures, n, oldFailures, n, rowCount, maxSeq,
				rowCount == maxSeq, followUpCode)

			if newFailures > 0 {
				t.Errorf("on %s: %d of %d new-code appends failed while old-binary writers shared the "+
					"stream -- this is what the outer retry in handleAppend exists to prevent", be.Name, newFailures, n)
			}
			if oldFailures > 0 {
				t.Errorf("on %s: %d of %d old-binary writes failed -- their own pre-existing retry loop "+
					"should already cover collisions among themselves", be.Name, oldFailures, n)
			}
			if rowCount != maxSeq {
				t.Errorf("on %s: %d rows but max sequence %d -- a duplicate or a gap", be.Name, rowCount, maxSeq)
			}
			if followUpCode != 201 {
				t.Errorf("on %s: the follow-up append after the mixed burst got %d, want 201: %s",
					be.Name, followUpCode, followUpBody)
			}
		})
	}
}
