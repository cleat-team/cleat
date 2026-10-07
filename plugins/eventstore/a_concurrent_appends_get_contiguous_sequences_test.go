// TestConcurrentAppendsGetContiguousSequences pins cleat#2260/#2268's
// concurrency requirement for the append path: N concurrent appenders to one
// stream must all succeed, with no duplicate or skipped sequence numbers.
// cleat#2260 first made this safe with a read-MAX-then-insert shape plus a
// retry loop on duplicate-key errors; cleat#2268 replaced that with
// upsertStreamHead's per-stream row lock (queries.go), which serializes
// concurrent appenders to the SAME stream instead of racing them -- see that
// comment for the mechanism. This test drives N real concurrent HTTP
// requests at one stream and asserts the result is exactly what that design
// promises: every append succeeds, and the sequences it received are
// {1..N} -- no duplicates, no gaps, and (cleat#2268's actual point) no
// retries needed to get there.
package eventstore

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestConcurrentAppendsGetContiguousSequences(t *testing.T) {
	const n = 20

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

			streamID := "concurrent-test-" + uuid.New().String()

			var wg sync.WaitGroup
			codes := make([]int, n)
			bodies := make([]string, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					req := httptest.NewRequest("POST", "/events/"+streamID,
						strings.NewReader(`{"n":1}`)).WithContext(tenantCtx)
					req.SetPathValue("stream_id", streamID)
					rec := httptest.NewRecorder()
					p.handleAppend(rec, req)
					codes[i] = rec.Code
					bodies[i] = rec.Body.String()
				}(i)
			}
			wg.Wait()

			for i, code := range codes {
				if code != http.StatusCreated {
					t.Fatalf("append %d on %s: want 201, got %d: %s", i, be.Name, code, bodies[i])
				}
			}

			// The row count and the sequence set, read back independently of
			// what the handler returned -- what matters is what actually
			// landed in the table, not what each response claimed.
			// Through p.db (SQLDBAdapter), not be.DB directly: MSSQL's
			// row-level-security filter predicate requires the session's
			// cleat.tenant_id context, which only SQLDBAdapter's
			// tenant-scoped transaction sets. A bare be.DB read saw 0 rows
			// here even though all 20 appends (through p.db) had just
			// succeeded -- Postgres's superuser test connection bypasses
			// RLS by default and MySQL has no RLS at all, so only MSSQL's
			// raw read was actually filtered to empty.
			rows, err := p.db.Query(tenantCtx,
				`SELECT sequence FROM event_stream WHERE tenant_id = $1 AND stream_id = $2 ORDER BY sequence ASC`,
				tenantID, streamID)
			if err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			defer rows.Close()

			var seqs []int64
			for rows.Next() {
				var s int64
				if err := rows.Scan(&s); err != nil {
					t.Fatalf("scan on %s: %v", be.Name, err)
				}
				seqs = append(seqs, s)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows iteration on %s: %v", be.Name, err)
			}

			if len(seqs) != n {
				t.Fatalf("on %s: %d rows landed, want %d -- a concurrent append was lost or duplicated: %v",
					be.Name, len(seqs), n, seqs)
			}
			for i, s := range seqs {
				want := int64(i + 1)
				if s != want {
					t.Errorf("on %s: sequences = %v, want a contiguous 1..%d run (first mismatch at "+
						"index %d: got %d, want %d)", be.Name, seqs, n, i, s, want)
					break
				}
			}
		})
	}
}
