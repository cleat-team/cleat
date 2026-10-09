// TestConcurrentPutsToABrandNewKeyDoNotRace is the falsification cleat#2890
// asks for before assuming cleat#2268's HOLDLOCK fix transfers unchanged:
// upsertKV (queries.go) is the same per-row MERGE shape as eventstore's
// upsertStreamHead, and PR #2885's regression test proved that shape races
// on SQL Server without WITH (HOLDLOCK) -- two concurrent MERGE statements
// against the SAME not-yet-existing key can both evaluate "WHEN NOT MATCHED"
// true under READ COMMITTED (neither sees the other's uncommitted insert)
// and both attempt the INSERT branch.
//
// kvstore's handlePut has no retry loop around upsertKV (unlike
// eventstore's handleAppend, which retries on a PK-conflict), so a losing
// racer here cannot self-heal: it would surface the raw duplicate-key error
// straight to the caller as a 500. That makes this test's discriminator
// sharper than eventstore's own -- "no 500s" -- rather than needing to read
// back a sequence set.
//
// This test is dialect-agnostic in what it asserts (every backend
// NewPluginTestBackends returns is exercised), but the hazard itself is
// MSSQL-specific: PostgreSQL's ON CONFLICT and MySQL's ON DUPLICATE KEY
// UPDATE both take the row's gap/next-key lock before deciding which branch
// to take, so neither can produce this race. MSSQL's MERGE is the one
// dialect where the decision and the lock are separate steps.
package kvstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestConcurrentPutsToABrandNewKeyDoNotRace(t *testing.T) {
	// n=20 through this HTTP+tenant-tx path never reproduced the race in
	// measurement (10 rounds, 0 failures): the extra round trips each
	// request pays before its MERGE (BEGIN, sp_set_session_context) stagger
	// the n goroutines enough that the sub-millisecond "both evaluated WHEN
	// NOT MATCHED before either inserted" window is rarely hit, even with a
	// start barrier. A raw probe issuing the same MERGE directly over bare
	// *sql.Conns (no transaction, no session context) confirmed the hazard
	// is real on this exact statement -- 80 of 1200 attempts (30 rounds of
	// 40) got error 2627 without WITH (HOLDLOCK), 0 of 1200 with it -- so
	// n=20 here was underpowered, not evidence of no hazard. n=60 is what
	// reproduces it reliably through the full path; see this test's own
	// falsification note in the PR description for the round-by-round count.
	const n = 60

	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: slog.Default(), config: Config{MaxValueSize: 1 << 20}}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.mux = http.NewServeMux()
			if err := p.RegisterRoutes(p.mux.(*http.ServeMux)); err != nil {
				t.Fatalf("RegisterRoutes: %v", err)
			}

			// Unique per run, not just per dialect: these backends are real,
			// persistent database containers (not recreated per `go test`
			// invocation), and SetupFullSchema's DDL is idempotent CREATE-IF-
			// NOT-EXISTS -- it does not clear prior rows. A fixed key name
			// collided with leftover rows from an earlier run and turned
			// "exactly one 201" into a guaranteed false failure on every
			// dialect, including Postgres and MySQL where no race is even
			// possible -- caught by running this with -count=10 before
			// trusting a single green or red result.
			key := "concurrent-new-key-" + be.Name + "-" + uuid.New().String()

			// start is closed once every goroutine is already blocked on it,
			// so all n racers issue their MERGE as close to simultaneously as
			// the Go scheduler allows -- spawning them in a loop and letting
			// each run immediately leaves enough stagger between requests
			// (connection dial, tenant-tx BEGIN, sp_set_session_context) that
			// the narrow "both evaluated WHEN NOT MATCHED before either
			// inserted" window can be missed even when it is real. A
			// regression test that only sometimes reproduces its own defect
			// is a green that reads as coverage; see
			// TestConcurrentPutsToABrandNewKeyDoNotRace's package comment.
			start := make(chan struct{})
			var ready sync.WaitGroup
			var wg sync.WaitGroup
			codes := make([]int, n)
			bodies := make([]string, n)
			ready.Add(n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					req := beReq("PUT", "/kv/"+key, []byte(fmt.Sprintf(`{"writer":%d}`, i)))
					rec := httptest.NewRecorder()
					ready.Done()
					<-start
					p.mux.ServeHTTP(rec, req)
					codes[i] = rec.Code
					bodies[i] = rec.Body.String()
				}(i)
			}
			ready.Wait()
			close(start)
			wg.Wait()

			created, ok := 0, 0
			for i, code := range codes {
				switch code {
				case http.StatusCreated:
					created++
				case http.StatusOK:
					ok++
				default:
					t.Errorf("on %s: writer %d got %d, want 200 or 201 -- a losing MERGE racer surfaced "+
						"as an error instead of serializing: %s", be.Name, i, code, bodies[i])
				}
			}
			if created != 1 {
				t.Errorf("on %s: %d of %d concurrent writers got 201 Created, want exactly 1 -- "+
					"either two racers both took the MERGE's \"WHEN NOT MATCHED\" INSERT branch "+
					"(cleat#2890) or one was lost entirely", be.Name, created, n)
			}
			if created+ok != n {
				t.Fatalf("on %s: %d responses accounted for (%d created + %d ok), want %d -- "+
					"see the per-writer errors above", be.Name, created+ok, created, ok, n)
			}

			// Ground truth: read the row back directly, independent of what
			// the n responses individually claimed. If every PUT serialized
			// correctly, the stored version is exactly n -- one INSERT plus
			// (n-1) increments, no duplicates and no lost updates.
			req := beReq("GET", "/kv/"+key, nil)
			rec := httptest.NewRecorder()
			p.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("on %s: final GET of %q: want 200, got %d: %s", be.Name, key, rec.Code, rec.Body.String())
			}
			var getResp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
				t.Fatalf("on %s: decode final GET: %v", be.Name, err)
			}
			etag := rec.Header().Get("ETag")
			if etag != fmt.Sprintf("%d", n) {
				t.Errorf("on %s: final version (ETag) = %q, want %q -- the row's version does not "+
					"reflect all %d concurrent writers landing", be.Name, etag, fmt.Sprintf("%d", n), n)
			}
		})
	}
}
