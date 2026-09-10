// Package scheduler provides multi-backend behavioral tests that run the
// scheduler plugin against real database backends (PostgreSQL, MySQL, MSSQL).
// They complement the fake-driver tests in scheduler_behavioral_test.go, and
// the distinction is the point rather than a convenience.
//
// That suite drives the plugin through a driver.Conn that pattern-matches the
// query string and returns canned rows. A fake accepts any SQL -- including SQL
// no database would parse, and including SQL issued on a connection that is
// already busy -- so a suite built on one cannot see either defect cleat#1133
// turned up here. Both were found by putting a real database on the other end.
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestSchedulerClaimsADueSchedule_MultiBackend asserts that a schedule whose
// next_run_at has passed is found AND claimed on every available backend.
//
// It asserts the claim rather than the absence of an error because
// runDueSchedules logs its failures and returns (0, 0, 0). A query that does
// not parse is therefore indistinguishable from "nothing is due" to every
// caller, which is how cleat#1133 reached a nightly that reported success.
func TestSchedulerClaimsADueSchedule_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("scheduler migrations on %s: %v", be.Name, err)
			}

			// A tenant of this run's own. The due query is deliberately not
			// tenant-scoped -- the scheduler polls for every tenant -- so other
			// rows on a shared database are visible to it and the assertions
			// below are written to tolerate them. Nothing here deletes outside
			// this tenant.
			tenant := uuid.New()
			t.Cleanup(func() {
				_, _ = be.DB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM schedules WHERE tenant_id = $1`, dialect), tenant)
			})

			// Every value is bound as a parameter, so this INSERT carries no
			// dialect-specific expression and cannot reintroduce the defect it
			// exists to detect.
			due := time.Now().UTC().Add(-time.Hour)
			id := uuid.New()
			if _, err := be.DB.ExecContext(ctx, plugin.Rebind(`
				INSERT INTO schedules
					(tenant_id, id, name, cron, workflow_name, input, enabled, next_run_at, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`, dialect),
				tenant, id, "a-schedule-that-is-due", "* * * * *", "some-workflow",
				[]byte(`{}`), true, due, due, due); err != nil {
				t.Fatalf("insert a due schedule on %s: %v", be.Name, err)
			}

			// runDueSchedules reports every SQL failure to its logger and to
			// nothing else, so the log is the only place a diagnosis exists.
			var logbuf bytes.Buffer
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(&logbuf, nil))
			p.env = &plugin.Environment{
				StartWorkflow: func(context.Context, string, json.RawMessage) (string, error) {
					return "run-1", nil
				},
			}
			t.Cleanup(func() {
				if t.Failed() && logbuf.Len() > 0 {
					t.Logf("scheduler log on %s:\n%s", be.Name, logbuf.String())
				}
			})

			found, started, failed := p.runDueSchedules(ctx)
			if found < 1 {
				t.Errorf("a schedule whose next_run_at is an hour in the past was not found on %s: "+
					"runDueSchedules returned found=%d started=%d failed=%d",
					be.Name, found, started, failed)
			}

			// The exact assertion, on the row this test wrote: a claim advances
			// next_run_at to the cron's next occurrence. That covers the SELECT
			// and the UPDATE together, since the row must be returned for the
			// update to be attempted and the update must run for the stored
			// value to move.
			var after time.Time
			if err := be.DB.QueryRowContext(ctx,
				plugin.Rebind(`SELECT next_run_at FROM schedules WHERE id = $1`, dialect), id,
			).Scan(&after); err != nil {
				t.Fatalf("re-read the schedule on %s: %v", be.Name, err)
			}
			if !after.After(due) {
				t.Errorf("on %s the schedule was not claimed: next_run_at is still %s, unchanged "+
					"from the due time %s. Either the SELECT did not return the row, or the "+
					"UPDATE that follows it did not run.",
					be.Name, after.UTC(), due.UTC())
			}
		})
	}
}

// TestSchedulerAPIWritesAScheduleOnEveryBackend_MultiBackend covers the two
// statements in routes.go that carried now(): the INSERT behind
// POST /schedules and the dynamically-built UPDATE behind PUT /schedules/{id}.
//
// Neither is reachable from the poll test above, which does its own INSERT --
// so without this, the routes.go half of the cleat#1133 fix would ship with no
// test at all. On SQL Server both statements failed outright before the fix,
// which means a schedule could not be created or edited there, not merely that
// cron did not fire.
//
// Routed through a real ServeMux rather than by calling the handler directly,
// because handleUpdate reads r.PathValue("id") and that is set by the mux.
func TestSchedulerAPIWritesAScheduleOnEveryBackend_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("scheduler migrations on %s: %v", be.Name, err)
			}

			var logbuf bytes.Buffer
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(&logbuf, nil))

			tenant := uuid.New()
			t.Cleanup(func() {
				_, _ = be.DB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM schedules WHERE tenant_id = $1`, dialect), tenant)
				if t.Failed() && logbuf.Len() > 0 {
					t.Logf("scheduler log on %s:\n%s", be.Name, logbuf.String())
				}
			})

			mux := http.NewServeMux()
			if err := p.RegisterRoutes(mux); err != nil {
				t.Fatalf("RegisterRoutes on %s: %v", be.Name, err)
			}
			authed := func(method, target, body string) *http.Request {
				req := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
				return req.WithContext(auth.WithTenantID(context.Background(), tenant))
			}

			// POST /schedules -- the INSERT.
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, authed("POST", "/schedules",
				`{"name":"nightly","cron":"0 3 * * *","workflow_name":"some-workflow"}`))
			if rec.Code != 201 {
				t.Fatalf("POST /schedules on %s: status %d, body %s",
					be.Name, rec.Code, rec.Body.String())
			}
			var created struct {
				ID   uuid.UUID `json:"id"`
				Name string    `json:"name"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response on %s: %v (%s)", be.Name, err, rec.Body.String())
			}

			// PUT /schedules/{id} -- the dynamically-built UPDATE.
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, authed("PUT", "/schedules/"+created.ID.String(),
				`{"name":"renamed"}`))
			if rec.Code != 200 {
				t.Fatalf("PUT /schedules/{id} on %s: status %d, body %s",
					be.Name, rec.Code, rec.Body.String())
			}

			// A 200 from a handler that logs its errors is not evidence the row
			// changed -- read it back.
			var name string
			if err := be.DB.QueryRowContext(ctx,
				plugin.Rebind(`SELECT name FROM schedules WHERE id = $1`, dialect), created.ID,
			).Scan(&name); err != nil {
				t.Fatalf("re-read the created schedule on %s: %v", be.Name, err)
			}
			if name != "renamed" {
				t.Errorf("on %s the update did not land: name is %q, want %q", be.Name, name, "renamed")
			}
		})
	}
}
