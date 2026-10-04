// upsertQuery runs on SQL Server, through the handler that calls it.
//
// cleat#2920. This is the only one of the five that needed a harness written
// from nothing rather than one extended: ratelimiter had NO per-dialect DB test
// at all. The `DialectMSSQL` reference in ratelimiter_test.go is
// TestIsDuplicateKeyError, a unit test of the error classifier that never opens
// a connection -- so `upsertQuery`'s MSSQL arm, the one cleat#2904/#2915 gave a
// WITH (HOLDLOCK) to close the concurrent-PUT duplicate-key window, has never
// been executed by anything.
//
// It is driven through handlePut rather than by executing the statement, on
// purpose: handlePut is where the hint's own comment says the concurrency comes
// from ("plain HTTP request concurrency, handlePut has no serialization of its
// own"), and it is the only caller. Running the literal directly would test a
// statement and not the path the issue is about.
//
// Assertions are on the ROW, not on the response code: handlePut reports a
// failed upsert as 500, but it also updates the in-memory bucket and writes the
// JSON body regardless of nothing -- an assertion on the status alone would
// pass for a handler that never reached the database. The row is read through
// CrossTenantConn because SQL Server filters a tenant-scoped read to nothing
// SILENTLY through be.DB.
package ratelimiter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestUpsertQueryRunsOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			db := &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p := &Plugin{}
			if err := p.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: quiet, DB: db}); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			key := "cleat2920-" + uuid.New().String()

			put := func(t *testing.T, maxReq, window int) *httptest.ResponseRecorder {
				t.Helper()
				body := `{"max_requests":` + strconv.Itoa(maxReq) + `,"window_seconds":` + strconv.Itoa(window) + `}`
				req := httptest.NewRequest(http.MethodPut, "/rate-limits/"+key, strings.NewReader(body))
				req = req.WithContext(auth.WithTenantID(req.Context(), tenantID))
				// handlePut reads the key from the route pattern, not the path,
				// so a request built by hand has to carry it explicitly.
				req.SetPathValue("key", key)
				rec := httptest.NewRecorder()
				p.handlePut(rec, req)
				return rec
			}

			if rec := put(t, 10, 60); rec.Code != http.StatusOK {
				t.Fatalf("handlePut on %s: status %d, body %s", be.Name, rec.Code, rec.Body.String())
			}

			readConn := be.CrossTenantConn(t, ctx,
				"cleat#2920: reading rate_limits, which SQL Server filters silently through be.DB")
			var maxReq, window int
			if err := readConn.QueryRowContext(ctx,
				plugin.Rebind(`SELECT max_requests, window_seconds FROM rate_limits WHERE tenant_id = $1 AND limit_key = $2`, dialect),
				tenantID, key).Scan(&maxReq, &window); err != nil {
				t.Fatalf("read rate_limits on %s: %v -- handlePut reported success (%d) but the row is not there",
					be.Name, err, http.StatusOK)
			}
			if maxReq != 10 || window != 60 {
				t.Errorf("upsertQuery on %s: row is (%d, %d), want (10, 60)", be.Name, maxReq, window)
			}

			// The UPDATE branch, which is the half the hint exists for: a second
			// PUT under the same (tenant_id, limit_key) must take WHEN MATCHED
			// rather than raise a duplicate key. A MERGE that lost its match
			// predicate would still pass the assertion above.
			if rec := put(t, 25, 30); rec.Code != http.StatusOK {
				t.Fatalf("second handlePut on %s: status %d, body %s", be.Name, rec.Code, rec.Body.String())
			}
			if err := readConn.QueryRowContext(ctx,
				plugin.Rebind(`SELECT max_requests, window_seconds FROM rate_limits WHERE tenant_id = $1 AND limit_key = $2`, dialect),
				tenantID, key).Scan(&maxReq, &window); err != nil {
				t.Fatalf("re-read rate_limits on %s: %v", be.Name, err)
			}
			if maxReq != 25 || window != 30 {
				t.Errorf("upsertQuery UPDATE branch on %s: row is (%d, %d), want (25, 30)", be.Name, maxReq, window)
			}
			var rows int
			if err := readConn.QueryRowContext(ctx,
				plugin.Rebind(`SELECT COUNT(*) FROM rate_limits WHERE tenant_id = $1 AND limit_key = $2`, dialect),
				tenantID, key).Scan(&rows); err != nil {
				t.Fatalf("count rate_limits on %s: %v", be.Name, err)
			}
			if rows != 1 {
				t.Errorf("upsertQuery on %s: two PUTs under one (tenant_id, limit_key) left %d rows, want 1", be.Name, rows)
			}
		})
	}
}
